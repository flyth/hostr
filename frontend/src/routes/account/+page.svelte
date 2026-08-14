<script lang="ts">
  import { onMount } from 'svelte';
  import { get, post, del, ago, type Token } from '$lib/api';
  import { session } from '$lib/session.svelte';

  let tokens = $state<Token[]>([]);
  let label = $state('');
  let fresh = $state('');
  let error = $state('');

  let oldPassword = $state('');
  let newPassword = $state('');
  let pwNotice = $state('');
  let pwError = $state('');

  async function load() {
    tokens = (await get<Token[]>('/api/tokens')) ?? [];
  }
  onMount(load);

  async function create() {
    error = '';
    try {
      const t = await post<{ token: string }>('/api/tokens', { name: label.trim() || 'hostrctl' });
      fresh = t.token;
      label = '';
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function revoke(id: string) {
    error = '';
    try {
      await del(`/api/tokens/${id}`);
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }

  async function changePassword(e: SubmitEvent) {
    e.preventDefault();
    pwError = '';
    pwNotice = '';
    try {
      await post('/api/password', { old: oldPassword, new: newPassword });
      oldPassword = '';
      newPassword = '';
      pwNotice = 'Password changed. Signing you out of every session…';
      setTimeout(() => session.logout(), 1200);
    } catch (err) {
      pwError = err instanceof Error ? err.message : String(err);
    }
  }
</script>

<section class="mb-8">
  <h2 class="mb-3 text-sm font-semibold tracking-wide uppercase">API tokens</h2>
  <p class="text-mute mb-3 text-xs">
    hostrctl authenticates with these. A token inherits your access to every site you own or
    collaborate on.
  </p>

  {#if fresh}
    <div class="card mb-3 border-sky-800 p-4">
      <p class="mb-2 text-xs">Copy this now — it is not shown again.</p>
      <code class="bg-ink block overflow-x-auto rounded p-2 font-mono text-sm">{fresh}</code>
      <button class="btn mt-2 text-xs" onclick={() => (fresh = '')}>Done</button>
    </div>
  {/if}

  <div class="mb-3 flex gap-2">
    <input class="input-field" bind:value={label} placeholder="laptop" />
    <button class="btn btn-primary" onclick={create}>New token</button>
  </div>
  {#if error}<p class="mb-3 text-sm text-red-400">{error}</p>{/if}

  <ul class="space-y-2">
    {#each tokens as t (t.id)}
      <li class="card flex items-center justify-between p-3 text-sm">
        <span>
          {t.name}
          <span class="text-mute text-xs">· created {ago(t.created)} · last used {ago(t.last_used)}</span>
        </span>
        <button class="btn btn-danger text-xs" onclick={() => revoke(t.id)}>Revoke</button>
      </li>
    {:else}
      <li class="text-mute text-sm">No tokens yet.</li>
    {/each}
  </ul>
</section>

<section>
  <h2 class="mb-3 text-sm font-semibold tracking-wide uppercase">Password</h2>
  <form class="card max-w-sm space-y-3 p-4" onsubmit={changePassword}>
    <label class="block space-y-1">
      <span class="text-mute text-xs">Current password</span>
      <input class="input-field" type="password" bind:value={oldPassword} required autocomplete="current-password" />
    </label>
    <label class="block space-y-1">
      <span class="text-mute text-xs">New password (10+ characters)</span>
      <input class="input-field" type="password" bind:value={newPassword} required minlength="10" autocomplete="new-password" />
    </label>
    {#if pwError}<p class="text-sm text-red-400">{pwError}</p>{/if}
    {#if pwNotice}<p class="text-accent text-sm">{pwNotice}</p>{/if}
    <button class="btn btn-primary">Change password</button>
  </form>
</section>
