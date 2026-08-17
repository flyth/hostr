<script lang="ts">
  import { untrack } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import {
    get,
    post,
    patch,
    del,
    humanBytes,
    ago,
    type Listing,
    type Site,
    type Stats,
    type Token,
  } from '$lib/api';
  import TokenLimits from '$lib/TokenLimits.svelte';
  import { session } from '$lib/session.svelte';
  import { mount as mountBrowser } from '$lib/browser.js';
  import '$lib/browser.css';

  const id = $derived(page.params.id!);

  let site = $state<Site | null>(null);
  let tokens = $state<Token[]>([]);
  let error = $state('');
  let notice = $state('');
  let domain = $state('');
  let collaborator = $state('');
  let authName = $state('');
  let authUser = $state('');
  let authPass = $state('');
  let aliasNote = $state('');
  let tokenScope = $state('');
  let tokenName = $state('');
  let newToken = $state('');
  let browserEl = $state<HTMLDivElement | null>(null);
  let browserDialog = $state<HTMLDialogElement | null>(null);
  let browserOpen = $state(false);

  const canManage = $derived(!!site && (site.mine || !!session.me?.admin));
  const collaborators = $derived(site?.collaborators ?? []);
  const aliases = $derived(site?.aliases ?? []);
  const accounts = $derived(site?.accounts ?? []);

  // `site.stats` is the site's own domain and nothing else, so the figure for
  // every hostname is a sum. Deriving it by subtraction instead would start
  // lying the moment one guest link was reset or revoked: its traffic would
  // vanish from the guest side and reappear as main-domain traffic that never
  // happened.
  const allTraffic = $derived.by((): Stats => {
    const own = site?.stats ?? {};
    return aliases.reduce(
      (acc, a) => ({
        count: (acc.count ?? 0) + (a.stats.count ?? 0),
        bytes: (acc.bytes ?? 0) + (a.stats.bytes ?? 0),
        first: acc.first,
        last: acc.last,
      }),
      { count: own.count ?? 0, bytes: own.bytes ?? 0, first: own.first, last: own.last },
    );
  });

  const hits = (s: Stats) => `${s.count ?? 0} ${(s.count ?? 0) === 1 ? 'hit' : 'hits'}`;

  async function copy(text: string) {
    error = '';
    try {
      await navigator.clipboard.writeText(text);
      notice = 'Copied.';
    } catch {
      notice = text; // clipboard blocked (insecure origin, denied permission)
    }
  }

  async function load(siteId: string) {
    error = '';
    try {
      site = await get<Site>(`/api/sites/${siteId}`);
      domain = site.domain;
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  // Tokens are the caller's own — the endpoint never returns anyone else's, so
  // the site view is your tokens for this site, not every token that can reach
  // it. Filtered here rather than server side: the list is a handful of rows
  // the panel has already fetched for the account page.
  async function loadTokens(siteId: string) {
    let next: Token[] = [];
    try {
      next = ((await get<Token[]>('/api/tokens')) ?? []).filter((t) => t.site === siteId);
    } catch {
      // A failure here should not blank the page; the site itself still loaded.
    }
    // Two sites opened in quick succession race, and the slower response must
    // not paint one site's tokens onto the other's page.
    if (siteId === id) tokens = next;
  }

  // Keyed off `id` rather than onMount: SvelteKit reuses this component across
  // param changes, so a history or address-bar jump between two sites would
  // otherwise keep showing the first one.
  $effect(() => {
    load(id);
    loadTokens(id);
  });

  // The browser is a plain DOM module shared with the public listing pages, so
  // it is mounted by hand rather than composed as a component. It lives in a
  // dialog for the same reason the public page gives it the whole viewport: the
  // two panes size themselves against their container, and a short box inside a
  // scrolling page leaves them fighting each other for the scroll.
  $effect(() => {
    const el = browserEl;
    if (!el) return;
    const siteId = id;
    mountBrowser(el, {
      path: '',
      // Read untracked on purpose: this effect must depend on the element and
      // the site id and nothing else. Tracking `site` would remount the browser
      // — losing the directory the reader is standing in — every time a delete
      // or a settings change reloads the record.
      markdown: untrack(() => !!site?.markdown),
      list: (dir: string) => get<Listing>(`/api/sites/${siteId}/files?path=${encodeURIComponent(dir)}`),
      // Previews come back through the API rather than from the site's own
      // domain, so they work while the site is behind basic auth and never
      // render tenant HTML on this origin.
      href: (p: string) => `/api/sites/${siteId}/raw?path=${encodeURIComponent(p)}`,
      remove: async (p: string, isDir: boolean) => {
        await patch(`/api/sites/${siteId}/files`, { delete: [isDir ? `${p}/` : p] });
        await load(siteId);
      },
    });
  });

  // showModal rather than an open attribute: the backdrop, the focus trap and
  // Escape all come with it, and none of them are worth reimplementing.
  function openBrowser() {
    browserOpen = true;
    browserDialog?.showModal();
  }

  async function act(fn: () => Promise<unknown>, msg = '') {
    error = '';
    notice = '';
    try {
      await fn();
      notice = msg;
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
    // Reloaded whether or not it worked. A failure usually means the server
    // disagrees about what is here — a guest link already revoked in another
    // tab — and leaving the stale row on screen next to the error makes the
    // page keep asserting something the server has just denied.
    await load(id);
  }

  const saveDomain = () =>
    act(() => patch(`/api/sites/${id}`, { domain: domain.trim() }), 'Domain updated.');

  const toggle = (field: 'listing' | 'listing_no_root' | 'markdown' | 'scoped_only', value: boolean) =>
    act(() => patch(`/api/sites/${id}`, { [field]: value }), 'Setting saved.');

  const addAccount = () =>
    act(async () => {
      await post(`/api/sites/${id}/accounts`, {
        name: authName.trim(),
        user: authUser.trim(),
        password: authPass,
      });
      authName = '';
      authUser = '';
      authPass = '';
    }, 'Account added.');

  const removeAccount = (accountId: string) =>
    act(() => del(`/api/sites/${id}/accounts/${accountId}`), 'Account removed.');

  const createAlias = () =>
    act(async () => {
      await post(`/api/sites/${id}/aliases`, { note: aliasNote.trim() });
      aliasNote = '';
    }, 'Guest link created.');

  const removeAlias = (aliasId: string) =>
    act(() => del(`/api/sites/${id}/aliases/${aliasId}`), 'Guest link removed.');

  const resetAlias = (aliasId: string) =>
    act(() => post(`/api/sites/${id}/aliases/${aliasId}/reset`), 'Counters reset.');

  const resetAccount = (accountId: string) =>
    act(() => post(`/api/sites/${id}/accounts/${accountId}/reset`), 'Counters reset.');

  // Resetting the site resets its guest links and accounts with it, so the
  // implicit main-domain figure cannot end up larger than the total.
  const resetSite = () =>
    act(() => post(`/api/sites/${id}/reset`), 'All counters for this site reset.');

  const addMember = () =>
    act(async () => {
      await post(`/api/sites/${id}/members`, { name: collaborator.trim() });
      collaborator = '';
    }, 'Collaborator added.');

  const removeMember = (userId: string) =>
    act(() => del(`/api/sites/${id}/members/${userId}`), 'Collaborator removed.');

  async function mintToken() {
    error = '';
    newToken = '';
    const scopes = tokenScope
      .split(',')
      .map((s) => s.trim().replace(/^\/+|\/+$/g, ''))
      .filter(Boolean);
    try {
      const res = await post<{ token: string }>('/api/tokens', {
        name: tokenName.trim() || 'agent',
        site: id,
        scopes,
      });
      newToken = res.token;
      tokenName = '';
      tokenScope = '';
      await loadTokens(id);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function revokeToken(tokenId: string) {
    error = '';
    try {
      await del(`/api/tokens/${tokenId}`);
      await loadTokens(id);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function destroy() {
    if (!site) return;
    if (
      !confirm(`Delete ${site.slug} (${site.domain}) and every file it serves? This cannot be undone.`)
    )
      return;
    try {
      await del(`/api/sites/${id}`);
      await goto('/');
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }
</script>

{#if error && !site}
  <p class="text-sm text-red-400">{error}</p>
  <a href="/" class="text-accent mt-4 inline-block text-sm">← back to sites</a>
{:else if site}
  <a href="/" class="text-mute hover:text-text mb-4 inline-block text-sm">← sites</a>

  <header class="mb-6">
    <h1 class="text-xl font-semibold">{site.slug}</h1>
    <p class="text-mute font-mono text-sm">
      <a href="https://{site.domain}" class="hover:text-accent" rel="noreferrer">{site.domain}</a>
    </p>
    <p class="text-mute mt-1 text-xs">
      v{site.version} · {site.files} files · {humanBytes(site.bytes)} · deployed {ago(site.deployed)}
      · owner {site.owner_name}
    </p>
  </header>

  {#if error}<p class="mb-4 text-sm text-red-400">{error}</p>{/if}
  {#if notice}<p class="text-accent mb-4 text-sm">{notice}</p>{/if}

  <section class="card mb-6 p-4">
    <h2 class="mb-3 text-sm font-semibold tracking-wide uppercase">Deploy</h2>
    <pre class="bg-ink border-edge overflow-x-auto rounded border p-3 text-xs">hostrctl deploy -site {site.slug} ./build
hostrctl deploy -site {site.slug} -scope my-website ./build
hostrctl push   -site {site.slug} -scope my-website render.png</pre>
    <p class="text-mute mt-2 text-xs">
      A plain deploy replaces the whole site. With <code>-scope</code> it replaces only that
      directory and leaves the rest alone. <code>push</code> uploads single files and deletes
      nothing.
    </p>
  </section>

  <section class="card mb-6 flex flex-wrap items-center justify-between gap-3 p-4">
    <div>
      <h2 class="text-sm font-semibold tracking-wide uppercase">Files</h2>
      <p class="text-mute mt-1 text-xs">
        {site.files} files · {humanBytes(site.bytes)} · browse, preview and delete.
      </p>
    </div>
    <button class="btn" onclick={openBrowser}>Open file browser</button>
  </section>

  <dialog
    class="browser-modal"
    bind:this={browserDialog}
    onclose={() => (browserOpen = false)}
  >
    {#if browserOpen}
      <div class="bg-panel border-edge flex h-full flex-col overflow-hidden rounded border">
        <div class="border-edge flex items-center justify-between gap-3 border-b p-3">
          <h2 class="text-sm font-semibold tracking-wide uppercase">{site.slug} files</h2>
          <button class="btn text-xs" onclick={() => browserDialog?.close()}>Close</button>
        </div>
        <div class="min-h-0 flex-1" bind:this={browserEl}></div>
      </div>
    {/if}
  </dialog>

  {#if canManage}
    <section class="card mb-6 space-y-4 p-4">
      <h2 class="text-sm font-semibold tracking-wide uppercase">Settings</h2>
      <div class="flex flex-wrap items-end gap-3">
        <label class="min-w-56 flex-1 space-y-1">
          <span class="text-mute text-xs">Domain</span>
          <input class="input-field font-mono" bind:value={domain} />
        </label>
        <button class="btn" onclick={saveDomain} disabled={domain.trim() === site.domain}>Save</button>
      </div>

      <label class="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          class="mt-1"
          checked={site.listing}
          onchange={(e) => toggle('listing', e.currentTarget.checked)}
        />
        <span>
          Directory listing
          <span class="text-mute block text-xs">
            Serves a file browser wherever there is no index.html. This makes every path in the
            site discoverable — leave it off if anything here is only protected by having an
            unguessable URL.
          </span>
        </span>
      </label>

      {#if site.listing}
        <label class="ml-6 flex items-start gap-2 text-sm">
          <input
            type="checkbox"
            class="mt-1"
            checked={site.listing_no_root}
            onchange={(e) => toggle('listing_no_root', e.currentTarget.checked)}
          />
          <span>
            Except the root folder
            <span class="text-mute block text-xs">
              Browsing starts at a directory someone already knows. The root stays a 404, so the
              site cannot be walked from the top.
            </span>
          </span>
        </label>
      {/if}

      <label class="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          class="mt-1"
          checked={site.markdown}
          onchange={(e) => toggle('markdown', e.currentTarget.checked)}
        />
        <span>
          Render markdown files
          <span class="text-mute block text-xs">
            A <code>.md</code> file opens as a typeset document instead of downloading, and previews
            here render the same way. <code>?raw</code> always returns the source. A rendered
            document runs any HTML in it on the site's own origin — the same trust an uploaded
            <code>.html</code> file already has.
          </span>
        </span>
      </label>

      <label class="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          class="mt-1"
          checked={site.scoped_only}
          onchange={(e) => toggle('scoped_only', e.currentTarget.checked)}
        />
        <span>
          Scope-only writes
          <span class="text-mute block text-xs">
            Every write must name a scope. No deploy can replace the whole site, and nothing can
            touch a file at the top level. Deleting one named scope is still allowed.
          </span>
        </span>
      </label>

      <div class="border-edge border-t pt-4">
        <span class="text-mute text-xs">Password accounts</span>
        <p class="text-mute mb-2 text-xs">
          A browser prompt in front of every page and listing. Give each person their own account
          and you can see who has been reading. Credentials travel in the clear unless the site is
          served over HTTPS.
        </p>
        <ul class="mb-2 space-y-1">
          {#each accounts as account (account.id)}
            <li class="border-edge flex flex-wrap items-center justify-between gap-2 border-t pt-2 text-sm">
              <span class="min-w-0">
                {account.name}
                <span class="text-mute block text-xs">
                  {hits(account.stats)} · {humanBytes(account.stats.bytes ?? 0)} · first {ago(
                    account.stats.first,
                  )} · last {ago(account.stats.last)}
                </span>
              </span>
              <span class="flex shrink-0 gap-2">
                <button class="btn text-xs" onclick={() => resetAccount(account.id)}>Reset</button>
                <button class="btn btn-danger text-xs" onclick={() => removeAccount(account.id)}>
                  Remove
                </button>
              </span>
            </li>
          {:else}
            <li class="text-mute text-sm">None — the site is public.</li>
          {/each}
        </ul>
        <div class="flex flex-wrap gap-2">
          <input class="input-field flex-1" bind:value={authName} placeholder="label, e.g. anna" />
          <input class="input-field flex-1" bind:value={authUser} placeholder="username" />
          <input
            class="input-field flex-1"
            type="password"
            bind:value={authPass}
            placeholder="password"
          />
          <button
            class="btn"
            onclick={addAccount}
            disabled={!authName.trim() || !authUser.trim() || !authPass}
          >
            Add
          </button>
        </div>
        <p class="text-mute mt-1 text-xs">
          Usernames are not listed here — a username is half of a basic-auth credential, so it
          stays on the server. The label is what you see. Removing the last account makes the site
          public again.
        </p>
      </div>

      <div class="border-edge border-t pt-4">
        <span class="text-mute text-xs">Guest links</span>
        <p class="text-mute mb-2 text-xs">
          A throwaway hostname serving this same site. Hand one to each person and you can tell
          their visits apart, and take a single link away without touching the others. A guest link
          grants nothing extra: a site behind a password is behind it here too.
        </p>
        <ul class="mb-2 space-y-1">
          {#each aliases as alias (alias.id)}
            <li class="border-edge flex flex-wrap items-center justify-between gap-2 border-t pt-2 text-sm">
              <span class="min-w-0">
                <a
                  href="https://{alias.domain}"
                  class="hover:text-accent font-mono break-all"
                  rel="noreferrer">{alias.domain}</a
                >
                {#if alias.note}<span class="text-mute">· {alias.note}</span>{/if}
                <span class="text-mute block text-xs">
                  {hits(alias.stats)} · {humanBytes(alias.stats.bytes ?? 0)} · first {ago(
                    alias.stats.first,
                  )} · last {ago(alias.stats.last)}
                </span>
              </span>
              <span class="flex shrink-0 gap-2">
                <button class="btn text-xs" onclick={() => copy(`https://${alias.domain}`)}>
                  Copy
                </button>
                <button class="btn text-xs" onclick={() => resetAlias(alias.id)}>Reset</button>
                <button class="btn btn-danger text-xs" onclick={() => removeAlias(alias.id)}>
                  Remove
                </button>
              </span>
            </li>
          {:else}
            <li class="text-mute text-sm">None.</li>
          {/each}
        </ul>
        <div class="flex flex-wrap gap-2">
          <input
            class="input-field flex-1"
            bind:value={aliasNote}
            placeholder="who is this for? (optional)"
          />
          <button class="btn" onclick={createAlias}>Create guest link</button>
        </div>
      </div>

      <div class="border-edge border-t pt-4">
        <span class="text-mute text-xs">Traffic</span>
        <p class="text-mute mb-2 text-xs">
          Requests served and bytes sent, counted after any password prompt. Counters are held in
          memory and written out every few seconds, so a hard restart can lose the last of them.
        </p>
        <ul class="space-y-1 text-sm">
          <li class="flex flex-wrap items-baseline justify-between gap-2">
            <span>All hostnames</span>
            <span class="text-mute text-xs">
              {hits(allTraffic)} · {humanBytes(allTraffic.bytes ?? 0)}
            </span>
          </li>
          <li class="flex flex-wrap items-baseline justify-between gap-2">
            <span class="font-mono">{site.domain}</span>
            <span class="text-mute text-xs">
              {hits(site.stats)} · {humanBytes(site.stats.bytes ?? 0)} · first {ago(
                site.stats.first,
              )} · last {ago(site.stats.last)}
            </span>
          </li>
        </ul>
        <button class="btn mt-3 text-xs" onclick={resetSite}>Reset all counters</button>
        <p class="text-mute mt-1 text-xs">
          This zeroes the guest links and the accounts too. Each one on its own can be reset from
          its own row.
        </p>
      </div>

      <div class="border-edge border-t pt-4">
        <span class="text-mute text-xs">Collaborators</span>
        <p class="text-mute mb-2 text-xs">They can deploy and read this site. Nothing else.</p>
        <ul class="mb-2 space-y-1">
          {#each collaborators as member (member.id)}
            <li class="flex items-center justify-between text-sm">
              <span>{member.name}</span>
              <button class="btn btn-danger text-xs" onclick={() => removeMember(member.id)}>
                Remove
              </button>
            </li>
          {:else}
            <li class="text-mute text-sm">None.</li>
          {/each}
        </ul>
        <div class="flex gap-2">
          <input class="input-field" bind:value={collaborator} placeholder="username" />
          <button class="btn" onclick={addMember} disabled={!collaborator.trim()}>Add</button>
        </div>
      </div>

      <div class="border-edge border-t pt-4">
        <span class="text-mute text-xs">Agent token</span>
        <p class="text-mute mb-2 text-xs">
          A token for this site. Give it scopes — comma separated — and it can only write inside
          them: not the rest of the site, not your other sites, and not the account itself.
        </p>
        <div class="flex flex-wrap gap-2">
          <input class="input-field flex-1" bind:value={tokenName} placeholder="label" />
          <input
            class="input-field flex-1 font-mono"
            bind:value={tokenScope}
            placeholder="my-website, drafts"
          />
          <button class="btn" onclick={mintToken}>Create</button>
        </div>
        {#if newToken}
          <p class="text-mute mt-2 text-xs">Shown once — copy it now.</p>
          <pre class="bg-ink border-edge mt-1 overflow-x-auto rounded border p-3 text-xs">{newToken}</pre>
        {/if}

        <ul class="mt-3 space-y-1">
          {#each tokens as t (t.id)}
            <li class="border-edge flex items-center justify-between gap-3 border-t pt-2 text-sm">
              <span class="min-w-0">
                {t.name}
                <span class="text-mute text-xs">· last used {ago(t.last_used)}</span>
                <span class="block"><TokenLimits token={t} /></span>
              </span>
              <button class="btn btn-danger shrink-0 text-xs" onclick={() => revokeToken(t.id)}>
                Revoke
              </button>
            </li>
          {:else}
            <li class="text-mute mt-2 text-xs">
              None of your tokens are bound to this site. Tokens belonging to other people are not
              listed here.
            </li>
          {/each}
        </ul>
      </div>

      <div class="border-edge border-t pt-4">
        <button class="btn btn-danger" onclick={destroy}>Delete site</button>
      </div>
    </section>
  {/if}
{/if}

<style>
  /* The browser sizes its panes against its container, so the dialog has to be
     a fixed height rather than one that grows with the listing. */
  .browser-modal {
    width: min(72rem, 94vw);
    height: 88vh;
    margin: auto;
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    overflow: visible;
  }

  .browser-modal::backdrop {
    background: rgb(0 0 0 / 0.6);
  }
</style>
