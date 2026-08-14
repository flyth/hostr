<script lang="ts">
  import { onMount } from 'svelte';
  import { get, post, humanBytes, ago, type Site } from '$lib/api';

  let sites = $state<Site[]>([]);
  let loading = $state(true);
  let slug = $state('');
  let domain = $state('');
  let error = $state('');
  let busy = $state(false);

  async function load() {
    loading = true;
    try {
      sites = (await get<Site[]>('/api/sites')) ?? [];
    } finally {
      loading = false;
    }
  }
  onMount(load);

  async function create(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      await post('/api/sites', { slug: slug.trim(), domain: domain.trim() });
      slug = '';
      domain = '';
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<section class="mb-8">
  <h2 class="mb-3 text-sm font-semibold tracking-wide uppercase">New site</h2>
  <form class="card flex flex-wrap items-end gap-3 p-4" onsubmit={create}>
    <label class="min-w-40 flex-1 space-y-1">
      <span class="text-mute text-xs">Name</span>
      <input class="input-field" bind:value={slug} placeholder="blog" required />
    </label>
    <label class="min-w-56 flex-2 space-y-1">
      <span class="text-mute text-xs">Domain</span>
      <input class="input-field" bind:value={domain} placeholder="blog.example.com" required />
    </label>
    <button class="btn btn-primary" disabled={busy}>Create</button>
    {#if error}<p class="w-full text-sm text-red-400">{error}</p>{/if}
  </form>
  <p class="text-mute mt-2 text-xs">
    Point the domain's DNS at this server. It becomes an upload target immediately.
  </p>
</section>

<section>
  <h2 class="mb-3 text-sm font-semibold tracking-wide uppercase">Your sites</h2>
  {#if loading}
    <p class="text-mute text-sm">loading…</p>
  {:else if sites.length === 0}
    <p class="text-mute text-sm">Nothing yet. Create a site above.</p>
  {:else}
    <ul class="space-y-2">
      {#each sites as site (site.id)}
        <li>
          <a href="/sites/{site.id}" class="card block p-4 hover:border-neutral-500">
            <div class="flex flex-wrap items-baseline justify-between gap-2">
              <span class="font-medium">{site.slug}</span>
              <span class="text-mute font-mono text-sm">{site.domain}</span>
            </div>
            <div class="text-mute mt-1 flex flex-wrap gap-x-4 text-xs">
              <span>v{site.version}</span>
              <span>{site.files} files</span>
              <span>{humanBytes(site.bytes)}</span>
              <span>deployed {ago(site.deployed)}</span>
              <span>{site.mine ? 'owner' : `shared by ${site.owner_name}`}</span>
            </div>
          </a>
        </li>
      {/each}
    </ul>
  {/if}
</section>
