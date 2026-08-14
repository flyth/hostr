<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { session } from '$lib/session.svelte';
  import Login from '$lib/Login.svelte';

  let { children } = $props();

  onMount(() => session.load());

  const nav = $derived(
    [
      { href: '/', label: 'Sites' },
      { href: '/account', label: 'Account' },
      ...(session.me?.admin ? [{ href: '/invites', label: 'Invites' }] : [])
    ].map((n) => ({ ...n, active: n.href === '/' ? page.url.pathname === '/' : page.url.pathname.startsWith(n.href) }))
  );
</script>

{#if !session.ready}
  <div class="text-mute grid min-h-screen place-items-center text-sm">loading…</div>
{:else if !session.me}
  <Login />
{:else}
  <div class="mx-auto max-w-4xl p-6">
    <header class="border-edge mb-8 flex items-center justify-between border-b pb-4">
      <nav class="flex items-center gap-4 text-sm">
        <span class="font-semibold">hostr</span>
        {#each nav as item (item.href)}
          <a href={item.href} class={item.active ? 'text-accent' : 'text-mute hover:text-text'}>
            {item.label}
          </a>
        {/each}
      </nav>
      <div class="text-mute flex items-center gap-3 text-sm">
        <span>{session.me.name}{session.me.admin ? ' · admin' : ''}</span>
        <button class="btn text-xs" onclick={() => session.logout()}>Sign out</button>
      </div>
    </header>
    {@render children()}
  </div>
{/if}
