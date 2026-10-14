<script>
  import { onMount } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { me, profile, logout } from '../stores/session.js';
  import { closeChats } from '../stores/chats.js';
  import { panicClear, endCall } from '../stores/call.store.js';
  import { soundSettings, play } from '../sounds.js';
  import { displayName } from '../contact.js';

  let settings = null;
  let saved = false;
  let error = '';
  let copied = false;
  let wipeText = '';

  onMount(async () => {
    try { settings = await App.GetSettings(); } catch (err) { error = errText(err); }
  });

  async function save() {
    error = '';
    try {
      await App.SetSettings({ ...settings, socketBufferMb: Number(settings.socketBufferMb) || 0 });
      settings = await App.GetSettings();
      saved = true;
      setTimeout(() => (saved = false), 1500);
    } catch (err) {
      error = errText(err);
    }
  }

  async function signOut() {
    await endCall();
    closeChats();
    await logout();
  }

  async function wipe() {
    error = '';
    try {
      await panicClear();
      await App.TriggerPanicButton();
    } catch (err) {
      error = errText(err);
    }
    closeChats();
    await logout();
  }

  async function copyKey() {
    copied = await copy($me.pubKey);
    setTimeout(() => (copied = false), 1500);
  }
</script>

<div class="page">
  <div class="stack">
    <h1>Settings</h1>
    {#if error}<p class="error small">{error}</p>{/if}

    <div class="card stack">
      <h2>Identity</h2>
      <div class="small muted">Profile</div>
      <div>{$profile}</div>
      <label class="name">Name shown to people who add you
        <input bind:value={$displayName} placeholder={$profile} maxlength="64" />
      </label>
      <div class="small muted">Your key (others add you with it)</div>
      <div class="row"><span class="mono grow">{$me.pubKey}</span><button class="ghost" on:click={copyKey}>{copied ? 'Copied' : 'Copy'}</button></div>
      <div><button on:click={signOut}>Sign out</button></div>
    </div>

    <div class="card stack">
      <h2>Sounds</h2>
      <label class="row">
        <input type="checkbox" bind:checked={$soundSettings.enabled} />
        <span>Sounds for messages, calls and buttons</span>
      </label>
      <label class="vol">Volume
        <input type="range" min="0" max="100" step="5" disabled={!$soundSettings.enabled}
          value={Math.round($soundSettings.volume * 100)}
          on:input={(e) => ($soundSettings.volume = e.target.value / 100)}
          on:change={() => play('receive')} />
      </label>
      <div class="row wrap">
        <span class="small muted">Listen:</span>
        {#each [['receive', 'Message'], ['send', 'Sent'], ['ring', 'Incoming call'], ['join', 'Join'], ['leave', 'Leave'], ['error', 'Error']] as [name, title]}
          <button class="ghost small" data-silent disabled={!$soundSettings.enabled} on:click={() => play(name)}>{title}</button>
        {/each}
      </div>
    </div>

    {#if settings}
      <div class="card stack">
        <h2>Network</h2>
        <label class="row">
          <input type="checkbox" bind:checked={settings.directSocket} />
          <span>Direct socket: skip NAT discovery for maximum speed (only for computers with a public IP)</span>
        </label>
        <label class="buf">UDP socket buffer, MB (1-256)
          <input type="number" min="1" max="256" bind:value={settings.socketBufferMb} />
        </label>
        <div class="row"><button on:click={save}>Save</button>{#if saved}<span class="small ok">Saved. Takes effect after restart.</span>{/if}</div>
      </div>
    {/if}

    <div class="card stack">
      <h2>Emergency lock</h2>
      <p class="small muted">
        Erases the decrypted keys from memory at once and stops the node. Everything on disk stays encrypted and can only
        be opened again with your password or recovery phrase. Type LOCK to enable the button.
      </p>
      <div class="row">
        <input class="confirm" bind:value={wipeText} placeholder="LOCK" />
        <button class="danger solid" disabled={wipeText !== 'LOCK'} on:click={wipe}>Lock now</button>
      </div>
    </div>
  </div>
</div>

<style>
  .buf { max-width: 280px; }
  .name { max-width: 320px; }
  .vol { max-width: 280px; }
  .confirm { width: 120px; }
</style>
