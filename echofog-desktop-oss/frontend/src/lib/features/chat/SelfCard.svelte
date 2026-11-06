<script>
  import { profile } from '../../stores/session.js';
  import { displayName } from '../../contact.js';
  import { view } from '../../stores/nav.js';
  import { netStatus } from '../../stores/net.js';
  import { callState } from '../../services/call.service.js';
  import { isCallActive, toggleMute, toggleDeafen } from '../../stores/call.store.js';
  import { t } from '../../i18n/index.js';
  import Avatar from '../../components/ui/Avatar.svelte';
  import Icon from '../../components/ui/Icon.svelte';

  /** The "you" card at the foot of a column: who I am, whether the network is up, the microphone and
   *  the headphones (while there is a call) and the way to settings. */
  $: myName = ($displayName || '').trim() || $profile || $t('layout.you');
  $: online = $netStatus.status === 'ONLINE' && $netStatus.peerCount > 0;
  $: s = $callState;
</script>

<div class="selfbar">
  <span class="sc-av">
    <Avatar id={$profile} name={myName} size={36} />
    <span class="sc-dot" class:on={online}></span>
  </span>
  <div class="sc-who">
    <b>{myName}</b>
    <span>{online ? $t('layout.statusOnline') : $t('layout.statusOffline')} · {$t('shell.peers', { n: $netStatus.peerCount })}</span>
  </div>
  {#if $isCallActive}
    <button class="ib" class:off={s.isMicMuted} title={s.isMicMuted ? $t('call.unmute') : $t('call.mute')} aria-label={s.isMicMuted ? $t('call.unmute') : $t('call.mute')} aria-pressed={s.isMicMuted} on:click={toggleMute}><Icon name={s.isMicMuted ? 'micOff' : 'mic'} size={17} /></button>
    <button class="ib" class:off={s.isDeafened} title={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')} aria-label={s.isDeafened ? $t('call.undeafen') : $t('call.deafen')} aria-pressed={s.isDeafened} on:click={toggleDeafen}><Icon name={s.isDeafened ? 'headphonesOff' : 'headphones'} size={17} /></button>
  {/if}
  <button class="ib" title={$t('layout.settings')} aria-label={$t('layout.settings')} on:click={() => view.set('settings')}><Icon name="settings" size={17} /></button>
</div>

<style>
  .selfbar { display: flex; align-items: center; gap: 4px; margin: 6px 10px 10px; padding: 8px 8px 8px 10px; border-radius: var(--r-xl); background: var(--surface-2); flex: none; }
  .sc-av { position: relative; flex: none; margin-right: 6px; }
  .sc-dot { position: absolute; right: -1px; bottom: -1px; width: 11px; height: 11px; border-radius: 50%; background: var(--faint); border: 2px solid var(--surface-2); }
  .sc-dot.on { background: var(--ok); }
  .sc-who { text-align: left; flex: 1; min-width: 0; display: flex; flex-direction: column; line-height: 1.25; }
  .sc-who b { font-size: var(--fs-sm); font-weight: var(--fw-semibold); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sc-who span { font-size: var(--fs-xs); color: var(--faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ib { width: 30px; height: 30px; padding: 0; border: none; border-radius: var(--r-md); display: grid; place-items: center; flex: none; background: transparent; color: var(--muted); }
  .ib:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .ib.off { color: var(--danger-fg); }
</style>
