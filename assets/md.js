// Renders the markdown page shell. The document is fetched from this page's own
// URL with `?raw`, so relative links and images in it resolve against the file's
// real location and nothing has to be rewritten.
//
// The HTML this produces is the site's own content on the site's own origin,
// which is the same trust an uploaded `.html` file already carries. The file
// browser's preview pane is the case that is not — it renders into a sandboxed
// frame instead, because there the page around it belongs to someone else.

(function () {
  var root = document.getElementById('md');
  if (!root) return;

  function fail(message) {
    root.replaceChildren();
    var p = document.createElement('p');
    p.className = 'md-error';
    p.textContent = message;
    root.append(p);
    root.hidden = false;
  }

  // Heading ids GitHub-style, so an anchor written by hand in the document —
  // or a link from another page — lands where its author expected.
  function slug(text, taken) {
    var base = text
      .toLowerCase()
      .trim()
      .replace(/[^\w\- ]+/g, '')
      .replace(/\s+/g, '-');
    if (!base) base = 'section';
    var id = base;
    for (var n = 1; taken[id]; n++) id = base + '-' + n;
    taken[id] = true;
    return id;
  }

  function decorate() {
    var taken = {};
    root.querySelectorAll('h1, h2, h3, h4, h5, h6').forEach(function (h) {
      if (!h.id) h.id = slug(h.textContent || '', taken);
      var a = document.createElement('a');
      a.className = 'md-anchor';
      a.href = '#' + h.id;
      a.textContent = '#';
      a.setAttribute('aria-label', 'Link to this section');
      h.prepend(a);
    });
    // A wide table scrolls inside its own box rather than stretching the page.
    root.querySelectorAll('table').forEach(function (t) {
      var box = document.createElement('div');
      box.className = 'md-table';
      t.replaceWith(box);
      box.append(t);
    });
    var h1 = root.querySelector('h1');
    if (h1 && h1.textContent) document.title = h1.textContent.trim();
  }

  fetch(location.pathname + '?raw', { headers: { accept: 'text/plain' } })
    .then(function (res) {
      if (!res.ok) throw new Error('HTTP ' + res.status);
      return res.text();
    })
    .then(function (text) {
      root.innerHTML = marked.parse(text, { gfm: true, breaks: false });
      decorate();
      root.hidden = false;
      // The hash was unresolvable while the document was still empty.
      if (location.hash) {
        var target = document.getElementById(decodeURIComponent(location.hash.slice(1)));
        if (target) target.scrollIntoView();
      }
    })
    .catch(function (err) {
      fail('Could not load this document: ' + (err && err.message ? err.message : err));
    });
})();
