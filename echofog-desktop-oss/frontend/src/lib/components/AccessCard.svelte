<script>
  // The Access card: whether this computer is listed for everybody (public) or answers only the
  // people it invited (hidden), the invitation link to give them, and who came in with it.
  import { onMount, onDestroy } from 'svelte';
  import QRCode from 'qrcode';
  import { App, errText, copy } from '../api.js';
  import { short, when } from '../format.js';
  import Icon from './ui/Icon.svelte';
  import { t, lang } from '../i18n/index.js';
  import { ask } from '../stores/dialog.js';

  export let contacts = [];

  $: MODES = ['public', 'hidden'].map((id) => ({ id, label: $t('access.mode.' + id), text: $t('access.mode.' + id + 'Text') }));

  let st = null;
  let error = '';
  let busy = '';
  let copied = false;
  let qr = '';
  let showQR = false;
  let timer;

  // the mode the user picked: the one saved for the next start, else the running one
  $: mode = st ? st.nextMode || st.mode : 'public';
  $: hidden = st && st.mode !== 'public';
  $: modeInfo = MODES.find((m) => m.id === mode);
  $: contactName = (id) => contacts.find((c) => (c.peer_id || '').toLowerCase() === id)?.alias || '';

  async function load() {
    try {
      st = await App.GetAccessStatus();
      error = '';
    } catch (err) {
      error = errText(err);
    }
  }

  async function run(what, fn) {
    busy = what;
    error = '';
    try {
      await fn();
      await load();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = '';
    }
  }

  const setMode = (id) => id !== mode && run('mode', () => App.SetAccessMode(id));
  const rotate = async () => {
    if (!(await ask({ message: $t('access.rotateConfirm'), confirm: $t('access.rotate') }))) return;
    run('rotate', async () => {
      await App.RotateAccessSecret();
      showQR = false;
    });
  };
  const remove = (id) => run('remove:' + id, () => App.RemoveJoinedPeer(id));

  async function copyLink() {
    copied = await copy(st.link);
    setTimeout(() => (copied = false), 1500);
  }

  let copiedWeb = false;
  async function copyWeb() {
    copiedWeb = await copy(st.webLink);
    setTimeout(() => (copiedWeb = false), 1500);
  }

  async function toggleQR() {
    showQR = !showQR;
    if (showQR && st?.link) qr = await QRCode.toDataURL(st.webLink || st.link, { margin: 1, width: 200, color: { dark: '#000000', light: '#ffffff' } });
  }

  onMount(() => {
    load();
    timer = setInterval(load, 10000);
  });
  onDestroy(() => clearInterval(timer));
</script>

<div class="card stack">
  <div class="card-head">
    <span class="ico" class:ok={hidden}><Icon name="lock" size={18} /></span>
    <div class="grow">
      <h2>{$t('access.title')}
        {#if hidden}<span class="chip ok live-chip">{$t('access.mode.' + st.mode).toLowerCase()}</span>{/if}
      </h2>
      <p class="small muted">{$t('access.hint')}</p>
    </div>
  </div>
  {#if error}<p class="small error">{error}</p>{/if}

  {#if st}
    {#if st.mode === 'isolated'}
      <p class="small muted">{$t('access.isolated')}</p>
    {:else}
      <div class="seg" role="radiogroup" aria-label={$t('access.modeLabel')}>
        {#each MODES as m}
          <button role="radio" aria-checked={mode === m.id} class:active={mode === m.id} disabled={!!busy} on:click={() => setMode(m.id)}>{m.label}</button>
        {/each}
      </div>
      <p class="small muted">{modeInfo.text}</p>
      {#if st.nextMode}<p class="small warn">{$t('access.restart')}</p>{/if}
    {/if}

    <div class="sub">
      <div class="eyebrow">{hidden ? $t('access.yourInvite') : $t('access.yourAddress')}</div>
      {#if st.link}
        <div class="copybox">
          <span class="mono grow link">{st.link}</span>
          <button class="icon" title={$t('access.copy')} on:click={copyLink}><Icon name={copied ? 'check' : 'copy'} size={16} /></button>
          <button class="icon" class:on={showQR} title={$t('access.showQR')} on:click={toggleQR}><Icon name="qr" size={16} /></button>
        </div>
        {#if st.webLink}
          <div class="eyebrow">{$t('links.web')}</div>
          <div class="copybox">
            <span class="mono grow link">{st.webLink}</span>
            <button class="icon" title={$t('links.copyWeb')} on:click={copyWeb}><Icon name={copiedWeb ? 'check' : 'copy'} size={16} /></button>
          </div>
          <p class="small faint">{$t('links.webNote')}</p>
        {/if}
        {#if showQR && qr}<img class="qr" src={qr} alt={$t('access.qrAlt')} width="200" height="200" />{/if}
        <p class="small faint">{hidden ? $t('access.inviteNote') : $t('access.addressNote')}</p>
      {:else}
        <p class="small faint">{$t('access.noLink')}</p>
      {/if}
      {#if hidden && !st.secretFromConfig}
        <div><button class="ghost small with-icon" on:click={rotate} disabled={!!busy}><Icon name="refresh" size={14} /> {busy === 'rotate' ? $t('common.working') : $t('access.rotate')}</button></div>
      {/if}
    </div>

    {#if hidden}
      <div class="sub">
        <div class="eyebrow">{$t('access.joined')}</div>
        {#if st.joined.length === 0}
          <p class="small faint">{$t('access.joinedEmpty')}</p>
        {/if}
        {#each st.joined as j (j.peerId)}
          <div class="peer-row">
            <span class="dot" class:on={j.connected}></span>
            <span class="grow">{#if contactName(j.peerId)}{contactName(j.peerId)} <span class="faint mono">· {short(j.peerId)}</span>{:else}<span class="mono">{short(j.peerId)}</span>{/if}
              <span class="small faint">· {when(j.joinedAt, $lang)}</span></span>
            <button class="icon tiny" title={$t('access.remove')} disabled={!!busy} on:click={() => remove(j.peerId)}><Icon name="trash" size={14} /></button>
          </div>
        {/each}
        <p class="small faint">{$t('access.joinedNote')}</p>
      </div>
    {/if}
  {:else if !error}
    <p class="small faint row"><span class="spinner"></span> {$t('common.loading')}</p>
  {/if}
</div>

<style>
  .sub { display: flex; flex-direction: column; gap: 8px; }
  .copybox { display: flex; align-items: center; gap: 6px; padding: 8px 6px 8px 12px; border-radius: 12px; background: var(--raised); }
  .link { word-break: break-all; font-size: 12px; }
  .qr { border-radius: 12px; align-self: flex-start; background: #fff; padding: 6px; }
  .peer-row { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border-radius: 12px; background: var(--raised); }
  .icon.tiny { width: 24px; height: 24px; border-radius: 7px; }
</style>
