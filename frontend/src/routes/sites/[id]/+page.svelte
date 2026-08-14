<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { get, post, patch, del, humanBytes, ago, type Manifest, type Site } from '$lib/api';
  import { session } from '$lib/session.svelte';

  const id = $derived(page.params.id!);

  let site = $state<Site | null>(null);
  let manifest = $state<Manifest | null>(null);
  let error = $state('');
  let notice = $state('');
  let domain = $state('');
  let collaborator = $state('');
  let showFiles = $state(false);

  const canManage = $derived(!!site && (site.mine || !!session.me?.admin));
  const files = $derived(
    Object.entries(manifest?.files ?? {}).sort(([a], [b]) => a.localeCompare(b))
  );
  const collaborators = $derived(site?.collaborators ?? []);

  async function load(siteId: string) {
    error = '';
    try {
      site = await get<Site>(`/api/sites/${siteId}`);
      domain = site.domain;
      manifest = await get<Manifest>(`/api/sites/${siteId}/manifest`);
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

  const saveDomain = () => act(() => patch(`/api/sites/${id}`, { domain: domain.trim() }), 'Domain updated.');

  const addMember = () =>
    act(async () => {
      await post(`/api/sites/${id}/members`, { name: collaborator.trim() });
      collaborator = '';
    }, 'Collaborator added.');

  const removeMember = (userId: string) =>
    act(() => del(`/api/sites/${id}/members/${userId}`), 'Collaborator removed.');

  async function destroy() {
    if (!site) return;
    if (!confirm(`Delete ${site.slug} (${site.domain}) and every file it serves? This cannot be undone.`))
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
    <pre class="bg-ink border-edge overflow-x-auto rounded border p-3 text-xs">hostrctl deploy -site {site.slug} ./build</pre>
    <p class="text-mute mt-2 text-xs">
      Only files whose contents changed are uploaded; anything missing from the directory is
      deleted from the site.
    </p>
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

      <div>
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
        <button class="btn btn-danger" onclick={destroy}>Delete site</button>
      </div>
    </section>
  {/if}

  <section class="card p-4">
    <button
      class="flex w-full items-center justify-between text-sm font-semibold tracking-wide uppercase"
      onclick={() => (showFiles = !showFiles)}
    >
      <span>Files ({files.length})</span>
      <span class="text-mute">{showFiles ? '−' : '+'}</span>
    </button>
    {#if showFiles}
      <ul class="mt-3 space-y-1 font-mono text-xs">
        {#each files as [path, entry] (path)}
          <li class="flex justify-between gap-4">
            <span class="truncate">{path}</span>
            <span class="text-mute shrink-0">{humanBytes(entry.size)}</span>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}
