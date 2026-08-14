<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { get, post, patch, del, humanBytes, ago, type Listing, type Site } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { mount as mountBrowser } from '$lib/browser.js';
  import '$lib/browser.css';

  const id = $derived(page.params.id!);

  let site = $state<Site | null>(null);
  let error = $state('');
  let notice = $state('');
  let domain = $state('');
  let collaborator = $state('');
  let authUser = $state('');
  let authPass = $state('');
  let tokenScope = $state('');
  let tokenName = $state('');
  let newToken = $state('');
  let browserEl = $state<HTMLDivElement | null>(null);

  const canManage = $derived(!!site && (site.mine || !!session.me?.admin));
  const collaborators = $derived(site?.collaborators ?? []);

  async function load(siteId: string) {
    error = '';
    try {
      site = await get<Site>(`/api/sites/${siteId}`);
      domain = site.domain;
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  // Keyed off `id` rather than onMount: SvelteKit reuses this component across
  // param changes, so a history or address-bar jump between two sites would
  // otherwise keep showing the first one.
  $effect(() => {
    load(id);
  });

  // The browser is a plain DOM module shared with the public listing pages, so
  // it is mounted by hand rather than composed as a component.
  $effect(() => {
    const el = browserEl;
    if (!el) return;
    const siteId = id;
    mountBrowser(el, {
      path: '',
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

  async function act(fn: () => Promise<unknown>, msg = '') {
    error = '';
    notice = '';
    try {
      await fn();
      notice = msg;
      await load(id);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  const saveDomain = () =>
    act(() => patch(`/api/sites/${id}`, { domain: domain.trim() }), 'Domain updated.');

  const toggle = (field: 'listing' | 'scoped_only', value: boolean) =>
    act(() => patch(`/api/sites/${id}`, { [field]: value }), 'Setting saved.');

  const saveAuth = () =>
    act(async () => {
      await patch(`/api/sites/${id}`, {
        auth_user: authUser.trim(),
        auth_password: authPass,
      });
      authPass = '';
    }, authUser.trim() ? 'Password protection is on.' : 'Password protection removed.');

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

  <section class="card mb-6 p-0">
    <h2 class="border-edge border-b p-4 text-sm font-semibold tracking-wide uppercase">Files</h2>
    <div class="h-[28rem]" bind:this={browserEl}></div>
  </section>

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
        <span class="text-mute text-xs">Password protection</span>
        <p class="text-mute mb-2 text-xs">
          A browser prompt in front of every page and listing. Credentials travel in the clear
          unless the site is served over HTTPS.
        </p>
        <div class="flex flex-wrap gap-2">
          <input class="input-field flex-1" bind:value={authUser} placeholder="username" />
          <input
            class="input-field flex-1"
            type="password"
            bind:value={authPass}
            placeholder={site.protected ? 'new password' : 'password'}
          />
          <button class="btn" onclick={saveAuth} disabled={!!authUser.trim() && !authPass}>
            {authUser.trim() ? 'Save' : 'Remove'}
          </button>
        </div>
        <p class="text-mute mt-1 text-xs">
          {site.protected
            ? 'Currently protected. The username is not shown here — it is half the credential, so it stays on the server. Re-enter both to change them.'
            : 'Currently public.'} Clear the username and save to remove protection.
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
      </div>

      <div class="border-edge border-t pt-4">
        <button class="btn btn-danger" onclick={destroy}>Delete site</button>
      </div>
    </section>
  {/if}
{/if}
