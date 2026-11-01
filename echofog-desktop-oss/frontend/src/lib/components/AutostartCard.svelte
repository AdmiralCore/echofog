<script>
  // "Start with Windows": EchoFog starts minimised at sign-in. With "remember on this computer"
  // the node is then back online by itself; without it the password screen waits. Off by default,
  // and not shown where the system cannot do it (only Windows can yet).
  import { onMount } from 'svelte';
  import { App, errText } from '../api.js';
  import Icon from './ui/Icon.svelte';
  import { t } from '../i18n/index.js';

  export let remembered = null; // this profile opens without its password (RememberCard); null until known

  let st = null; // what the Go side says (AutostartDTO)
  let want = false; // the switch
  let busy = false;
  let error = '';

  async function load() {
    try {
      st = await App.GetAutostart();
      want = st.enabled;
    } catch (err) {
      error = errText(err);
    }
  }

  async function set(on) {
    busy = true;
    error = '';
    try {
      await App.SetAutostart(on);
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
    await load();
  }

  onMount(load);
</script>

{#if st?.supported}
  <div class="card stack">
    <div class="card-head">
      <span class="ico"><Icon name="monitor" size={19} /></span>
      <div class="grow">
        <h2>{$t('autostart.title')}</h2>
        <p class="small muted">{$t('autostart.hint')}</p>
      </div>
      <input type="checkbox" class="switch" aria-label={$t('autostart.title')} bind:checked={want} on:change={() => set(want)} disabled={busy} />
    </div>
    {#if st.blocked}
      <p class="small warn">{$t('autostart.blocked')}</p>
    {:else if st.enabled && st.program}
      <div class="row wrap">
        <span class="small muted grow path">{$t('autostart.otherCopy', { path: st.program })}</span>
        <button class="small" on:click={() => set(true)} disabled={busy}>{$t('autostart.useThis')}</button>
      </div>
    {/if}
    {#if st.enabled && remembered === false}
      <p class="small muted">{$t('autostart.needsPassword')}</p>
    {/if}
    {#if error}<p class="small error">{error}</p>{/if}
  </div>
{/if}

<style>
  .path { overflow-wrap: anywhere; }
</style>
