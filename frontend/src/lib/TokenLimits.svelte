<script lang="ts">
  import type { Token } from '$lib/api';

  // What a token cannot do is the only interesting thing about it once it has
  // been minted, and it is the one thing the list used to leave out. Rendered
  // the same way on the account page and on a site, so the two can never drift
  // into describing the same limits differently.
  let { token }: { token: Token } = $props();

  const scopes = $derived(token.scopes ?? []);
  const where = $derived(token.site_slug ?? token.site ?? '');
</script>

{#if !token.site}
  <span class="text-mute text-xs">Full account access · every site you own or collaborate on</span>
{:else}
  <span class="text-mute text-xs">
    Site <code class="text-text">{where}</code>
    {#if scopes.length}
      · writes only under
      {#each scopes as scope, i}<code class="text-text">/{scope}/</code>{#if i < scopes.length - 1}<span>, </span
        >{/if}{/each}
    {:else}
      · anywhere in it
    {/if}
  </span>
{/if}
