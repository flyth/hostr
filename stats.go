package main

import (
	"net/http"
	"time"
)

// Traffic accounting. Every request a site serves bumps up to three counters —
// the site's own, the guest link it arrived on, and the basic-auth account that
// opened it — and none of them touch the disk. The store is marked dirty and a
// timer flushes it; a page with forty images is forty requests, and rewriting
// the whole store forty times is how a static host stops being fast.
//
// ponytail: the counters live in the store's own records under the store's own
// lock, so serving now takes one exclusive acquisition per request where it used
// to take a shared one. The critical section is a map lookup and a handful of
// adds — nanoseconds, against a request that is about to read a file off disk —
// and it buys no second concurrent structure to keep consistent with the first.
// Shard it into per-site atomics if a profile ever shows contention here, or if
// the store file grows past a megabyte and the flush itself starts to bite.

// statsFlush is how much counting a hard kill can cost. Nothing else depends on
// it: the numbers a client reads come from memory, so they are always current.
const statsFlush = 10 * time.Second

// Record folds one served request into the counters. aliasID is empty when the
// request came in on the site's own domain, accountID when the site has no
// password. counted=false marks a request that belongs to a visit already
// counted — see Stats.hit.
func (db *DB) Record(siteID, aliasID, accountID string, n int64, counted bool) {
	now := time.Now().UTC()
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.d.Sites[siteID]
	if !ok {
		// The site was deleted while its last response was still being written.
		return
	}
	// Exactly one hostname counter moves: the site's own, or the guest link the
	// request came in on. Nothing double-counts, so the total across a site is a
	// sum of things that are each independently resettable.
	if aliasID == "" {
		s.Stats.hit(now, n, counted)
	} else {
		for _, a := range s.Aliases {
			if a.ID == aliasID {
				a.Stats.hit(now, n, counted)
				break
			}
		}
	}
	if accountID != "" {
		for _, a := range s.Accounts {
			if a.ID == accountID {
				a.Stats.hit(now, n, counted)
				break
			}
		}
	}
	db.dirty = true
}

// Flush writes accumulated counters out, and does nothing at all if none have
// moved since the last write.
func (db *DB) Flush() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if !db.dirty {
		return nil
	}
	return db.save()
}

// countingWriter tallies the response body that actually reached the client,
// which is the only number that stays honest across a range request, a 304 and
// a download the visitor cancelled half way through.
//
// ponytail: wrapping the ResponseWriter hides its io.ReaderFrom, so a blob is
// copied through a 32 KiB buffer instead of going out via sendfile. Give this
// type a ReadFrom that counts and delegates if large-file throughput ever shows
// up as a problem.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}
