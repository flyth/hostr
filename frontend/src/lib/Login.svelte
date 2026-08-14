<script lang="ts">
  import { session } from '$lib/session.svelte';

  let mode = $state<'login' | 'invite'>('login');
  let name = $state('');
  let password = $state('');
  let code = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      if (mode === 'login') await session.login(name, password);
      else await session.register(code.trim(), name, password);
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="grid min-h-screen place-items-center p-6">
  <form class="card w-full max-w-sm space-y-4 p-6" onsubmit={submit}>
    <h1 class="text-lg font-semibold">hostr</h1>

    <div class="flex gap-1 text-sm">
      <button
        type="button"
        class="btn flex-1 {mode === 'login' ? 'btn-primary' : ''}"
        onclick={() => (mode = 'login')}>Sign in</button
      >
      <button
        type="button"
        class="btn flex-1 {mode === 'invite' ? 'btn-primary' : ''}"
        onclick={() => (mode = 'invite')}>Use an invite</button
      >
    </div>

    {#if mode === 'invite'}
      <label class="block space-y-1">
        <span class="text-mute text-xs">Invite code</span>
        <input class="input-field font-mono" bind:value={code} required autocomplete="off" />
      </label>
    {/if}

    <label class="block space-y-1">
      <span class="text-mute text-xs">Username</span>
      <input
        class="input-field"
        bind:value={name}
        required
        autocapitalize="none"
        autocomplete="username"
      />
    </label>

    <label class="block space-y-1">
      <span class="text-mute text-xs">Password</span>
      <input
        class="input-field"
        type="password"
        bind:value={password}
        required
        autocomplete={mode === 'login' ? 'current-password' : 'new-password'}
      />
    </label>

    {#if error}
      <p class="text-sm text-red-400">{error}</p>
    {/if}

    <button class="btn btn-primary w-full" disabled={busy}>
      {mode === 'login' ? 'Sign in' : 'Create account'}
    </button>
  </form>
</div>
