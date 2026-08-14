<script lang="ts">
  import { onMount } from 'svelte';
  import { get, post, del, ago, type Invite } from '$lib/api';

  let invites = $state<Invite[]>([]);
  let error = $state('');

  async function load() {
    try {
      invites = ((await get<Invite[]>('/api/invites')) ?? []).sort((a, b) =>
        b.created.localeCompare(a.created)
      );
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  }
  onMount(load);

  const create = async () => {
    error = '';
    try {
      await post('/api/invites');
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  };

  const revoke = async (code: string) => {
    error = '';
    try {
      await del(`/api/invites/${code}`);
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    }
  };
</script>

<section>
  <div class="mb-3 flex items-center justify-between">
    <h2 class="text-sm font-semibold tracking-wide uppercase">Invites</h2>
    <button class="btn btn-primary" onclick={create}>New invite</button>
  </div>
  <p class="text-mute mb-4 text-xs">
    Each code is good for one account. Hand it over out of band; the recipient redeems it on the
    sign-in screen.
  </p>

  {#if error}<p class="mb-3 text-sm text-red-400">{error}</p>{/if}

  <ul class="space-y-2">
    {#each invites as inv (inv.code)}
      <li class="card flex flex-wrap items-center justify-between gap-2 p-3 text-sm">
        <code class="font-mono {inv.used_by ? 'text-mute line-through' : ''}">{inv.code}</code>
        <span class="text-mute text-xs">
          created {ago(inv.created)}{inv.used_by ? ` · redeemed ${ago(inv.used_at)}` : ' · unused'}
        </span>
        <button class="btn btn-danger text-xs" onclick={() => revoke(inv.code)}>Delete</button>
      </li>
    {:else}
      <li class="text-mute text-sm">No invites. Create one to add a friend.</li>
    {/each}
  </ul>
</section>
