<script>
  import { onMount } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { me, profile, logout } from '../stores/session.js';
  import { closeChats } from '../stores/chats.js';
  import { panicClear, endCall } from '../stores/call.store.js';
  import { soundSettings, play } from '../sounds.js';
  import { callService, callState } from '../services/call.service.js';
  import { displayName } from '../contact.js';
  import { theme } from '../stores/theme.js';
  import { backdropSettings } from '../stores/backdrops.js';
  import { selfId } from '../stores/peers.store.js';
  import { reducedMotion } from '../components/call/backdrops/loop.js';
  import BackdropPicker from '../components/call/BackdropPicker.svelte';
  import Icon from '../components/ui/Icon.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import './skin.css';
  import Button from '../components/ui/Button.svelte';
  import IconButton from '../components/ui/IconButton.svelte';
  import Input from '../components/ui/Input.svelte';
  import Tabs from '../components/ui/Tabs.svelte';
  import Toggle from '../components/ui/Toggle.svelte';
  import { feedPref, COMPACT_ABOVE } from '../stores/feed.js';
  import { focus } from '../stores/nav.js';
  import BootstrapCard from '../components/BootstrapCard.svelte';
  import GuardCard from '../components/GuardCard.svelte';
  import ProxyCard from '../components/ProxyCard.svelte';
  import AnonymityCard from '../components/AnonymityCard.svelte';
  import { openLinkDialog } from '../links.js';
  import PrivacyCard from '../components/PrivacyCard.svelte';
  import RememberCard from '../components/RememberCard.svelte';
  import PasswordCard from '../components/PasswordCard.svelte';
  import AutostartCard from '../components/AutostartCard.svelte';
  import ProtocolCard from '../components/ProtocolCard.svelte';
  import Logs from './Logs.svelte';
  import { t, locale, LANGUAGES } from '../i18n/index.js';

  const THEMES = [
    { id: 'system', label: 'settings.themeSystem', icon: 'monitor' },
    { id: 'light', label: 'settings.themeLight', icon: 'sun' },
    { id: 'dark', label: 'settings.themeDark', icon: 'moon' },
  ];

  // groups of sections on the left; a section belongs to one group. `kw` are the i18n keys whose
  // texts the search looks through (the section's title and hint, plus words people would type).
  const GROUPS = [
    { id: 'profile', label: 'settings.g.profile', icon: 'users' },
    { id: 'look', label: 'settings.g.look', icon: 'palette' },
    { id: 'calls', label: 'settings.g.calls', icon: 'video' },
    { id: 'privacy', label: 'settings.g.privacy', icon: 'shield' },
    { id: 'network', label: 'settings.g.network', icon: 'globe' },
    { id: 'app', label: 'settings.g.app', icon: 'settings' },
    { id: 'diag', label: 'settings.g.diag', icon: 'terminal' },
  ];
  const SECTIONS = [
    { id: 'profile', g: 'profile', kw: ['settings.g.profile', 'settings.displayName', 'settings.yourKey', 'settings.signOut'] },
    { id: 'appearance', g: 'look', kw: ['settings.appearance', 'settings.appearanceHint', 'settings.theme', 'settings.themeSystem', 'settings.themeLight', 'settings.themeDark', 'settings.feedTitle'] },
    { id: 'language', g: 'look', kw: ['settings.language', 'settings.languageHint'] },
    { id: 'sounds', g: 'look', kw: ['settings.sounds', 'settings.soundsHint', 'settings.volume'] },
    { id: 'callbg', g: 'calls', kw: ['settings.callBg', 'settings.callBgHint'] },
    { id: 'screen', g: 'calls', kw: ['settings.screen', 'settings.screenHint'] },
    { id: 'privacy', g: 'privacy', kw: ['settings.privacy', 'settings.privacyHint', 'settings.blocked'] },
    { id: 'password', g: 'privacy', kw: ['password.title'] },
    { id: 'remember', g: 'privacy', kw: ['remember.title'] },
    { id: 'lock', g: 'privacy', kw: ['settings.lockTitle', 'settings.lockHint'] },
    { id: 'network', g: 'network', kw: ['settings.network', 'settings.networkHint', 'settings.directSocket'] },
    { id: 'anon', g: 'network', kw: ['anon.title'] },
    { id: 'guard', g: 'network', kw: ['guard.title'] },
    { id: 'proxy', g: 'network', kw: ['netproxy.title'] },
    { id: 'protocol', g: 'network', kw: ['protocol.title', 'protocol.hint'] },
    { id: 'boot', g: 'network', kw: ['boot.title'] },
    { id: 'logs', g: 'diag', kw: ['settings.g.diag', 'logs.title', 'logs.subtitle', 'report.open'] },
    { id: 'autostart', g: 'app', kw: ['autostart.title'] },
    { id: 'links', g: 'app', kw: ['links.title', 'links.hint'] },
  ];

  // the offline banner sends people to the node-address card: open its group
  let active = $focus === 'bootstrap' ? 'network' : $focus === 'diagnostics' ? 'diag' : 'profile';
  let query = '';
  $: q = query.trim().toLowerCase();
  $: searching = q.length > 0;
  $: hits = new Set(SECTIONS.filter((x) => x.kw.some((k) => $t(k).toLowerCase().includes(q))).map((x) => x.id));
  $: shown = (id) => (searching ? hits.has(id) : SECTIONS.find((x) => x.id === id).g === active);
  $: anyShown = hits.size > 0;
  function pick(id) { query = ''; active = id; }

  let settings = null;
  let saved = false;
  let error = '';
  let copied = false;
  let wipeText = '';
  let remembered = null; // this profile opens without its password: the start-with-Windows card says what that changes

  onMount(async () => {
    callService.checkNativeScreen();
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

<div class="page skin st">
  <header class="head">
    <div class="grow">
      <h1>{$t('settings.title')}</h1>
      <p class="muted small">{$t('settings.subtitle')}</p>
    </div>
  </header>
  {#if error}<p class="error small" role="alert">{error}</p>{/if}

  <div class="layout">
    <aside class="side">
      <Input size="sm" icon="search" type="search" aria-label={$t('settings.search')} placeholder={$t('settings.search')} bind:value={query} spellcheck="false" />
      <nav aria-label={$t('settings.groups')}>
        {#each GROUPS as g}
          <button class="g" class:on={!searching && active === g.id} aria-current={!searching && active === g.id ? 'page' : undefined} on:click={() => pick(g.id)}>
            <Icon name={g.icon} size={16} /> <span>{$t(g.label)}</span>
          </button>
        {/each}
      </nav>
    </aside>

    <div class="pane">
      {#if searching && !anyShown}
        <p class="muted none">{$t('settings.noResults')}</p>
      {/if}

      {#if shown('profile')}
        <section class="card stack" id="s-profile">
          <div class="idrow">
            <Avatar id={$profile} name={($displayName || '').trim() || $profile} size={48} />
            <div class="grow">
              <h2>{($displayName || '').trim() || $profile}</h2>
              <p class="small muted">{$t('settings.profileNamed', { name: $profile })}</p>
            </div>
            <Button icon="logout" on:click={signOut}>{$t('settings.signOut')}</Button>
          </div>
          <Input label={$t('settings.displayName')} bind:value={$displayName} placeholder={$profile} maxlength="64" />
          <div>
            <div class="lbl">{$t('settings.yourKey')}</div>
            <div class="copybox">
              <span class="mono grow">{$me.pubKey}</span>
              <IconButton icon={copied ? 'check' : 'copy'} label={$t('settings.copyKey')} size="sm" on:click={copyKey} />
            </div>
          </div>
        </section>
      {/if}

      {#if shown('appearance')}
        <section class="card stack" id="s-appearance">
          <div class="card-head">
            <span class="ico"><Icon name="palette" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.appearance')}</h2>
              <p class="small muted">{$t('settings.appearanceHint')}</p>
            </div>
          </div>
          <div class="themes" role="radiogroup" aria-label={$t('settings.theme')}>
            {#each THEMES as th}
              <button class="theme-opt" class:sel={$theme === th.id} role="radio" aria-checked={$theme === th.id} on:click={() => theme.set(th.id)}>
                <span class="preview {th.id}" aria-hidden="true">
                  <span class="pv-side"></span>
                  <span class="pv-main"><span class="pv-b in"></span><span class="pv-b out"></span><span class="pv-b in short"></span></span>
                </span>
                <span class="row opt-label"><Icon name={th.icon} size={14} /> {$t(th.label)}</span>
              </button>
            {/each}
          </div>
          <div class="field-row">
            <div class="grow">
              <h3>{$t('settings.feedTitle')}</h3>
              <p class="small muted">{$t('settings.feedHint', { n: COMPACT_ABOVE })}</p>
            </div>
            <Tabs variant="segment" label={$t('settings.feedTitle')} bind:value={$feedPref}
              items={[{ id: 'auto', label: $t('settings.feedAuto') }, { id: 'bubbles', label: $t('settings.feedBubbles') }, { id: 'compact', label: $t('settings.feedCompact') }]} />
          </div>
        </section>
      {/if}

      {#if shown('language')}
        <section class="card stack" id="s-language">
          <div class="card-head">
            <span class="ico"><Icon name="globe" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.language')}</h2>
              <p class="small muted">{$t('settings.languageHint')}</p>
            </div>
            <Tabs variant="segment" label={$t('settings.language')} value={$locale} on:change={(e) => locale.set(e.detail)}
              items={[{ id: 'system', label: $t('settings.langSystem') }, ...LANGUAGES.map((l) => ({ id: l.id, label: l.label }))]} />
          </div>
        </section>
      {/if}

      {#if shown('sounds')}
        <section class="card stack" id="s-sounds">
          <div class="card-head">
            <span class="ico"><Icon name="bell" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.sounds')}</h2>
              <p class="small muted">{$t('settings.soundsHint')}</p>
            </div>
            <Toggle label={$t('settings.sounds')} bind:checked={$soundSettings.enabled} />
          </div>
          <div class="vol row" class:off={!$soundSettings.enabled}>
            <Icon name="volumeOff" size={16} />
            <input class="grow" type="range" min="0" max="100" step="5" aria-label={$t('settings.volume')} disabled={!$soundSettings.enabled}
              value={Math.round($soundSettings.volume * 100)}
              on:input={(e) => ($soundSettings.volume = e.target.value / 100)}
              on:change={() => play('receive')} />
            <Icon name="volume" size={16} />
            <span class="pct">{Math.round($soundSettings.volume * 100)}%</span>
          </div>
          <div class="row wrap">
            <span class="small muted">{$t('settings.listen')}</span>
            {#each [['receive', 'settings.sndMessage'], ['send', 'settings.sndSent'], ['ring', 'settings.sndRing'], ['join', 'settings.sndJoin'], ['leave', 'settings.sndLeave'], ['error', 'settings.sndError']] as [name, title]}
              <Button size="sm" data-silent disabled={!$soundSettings.enabled} on:click={() => play(name)}>{$t(title)}</Button>
            {/each}
          </div>
        </section>
      {/if}

      {#if shown('callbg')}
        <section class="card stack" id="s-callbg">
          <div class="card-head">
            <span class="ico"><Icon name="image" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.callBg')}</h2>
              <p class="small muted">{$t('settings.callBgHint')}</p>
            </div>
          </div>
          <BackdropPicker peer={$selfId} />
          {#if $reducedMotion}<p class="small muted">{$t('settings.callBgStill')}</p>{/if}
          <Toggle label={$t('settings.callBgOthers')} bind:checked={$backdropSettings.others} />
        </section>
      {/if}

      {#if shown('screen')}
        <section class="card stack" id="s-screen">
          <div class="card-head">
            <span class="ico"><Icon name="screenUp" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.screen')}</h2>
              <p class="small muted">{$callState.nativeScreen.available ? $t('settings.screenHint') : $t('call.capture.unavailable', { reason: $callState.nativeScreen.reason || '…' })}</p>
            </div>
          </div>
          <Tabs variant="segment" label={$t('call.capture')} value={$callState.nativeScreen.available ? $callState.screenCapture : 'webview'} on:change={(e) => callService.setScreenCapture(e.detail)}
            items={[{ id: 'native', label: $t('call.capture.native'), disabled: !$callState.nativeScreen.available }, { id: 'webview', label: $t('call.capture.webview') }]} />
          <p class="small muted">{$callState.screenCapture === 'webview' || !$callState.nativeScreen.available ? $t('call.capture.webviewHint') : $t('call.capture.nativeHint')}</p>
          <Toggle label={$t('call.pick.sound')} hint={$t('call.pick.soundHint')} checked={$callState.screenSound}
            disabled={!$callState.nativeScreen.available || $callState.screenCapture !== 'native'}
            on:change={(e) => callService.setScreenSound(e.detail)} />
        </section>
      {/if}

      {#if shown('privacy')}<div id="s-privacy"><PrivacyCard /></div>{/if}
      {#if shown('password')}<div id="s-password"><PasswordCard /></div>{/if}
      {#if shown('remember')}<div id="s-remember"><RememberCard on:state={(e) => (remembered = e.detail.enabled)} /></div>{/if}
      {#if shown('autostart')}<div id="s-autostart"><AutostartCard {remembered} /></div>{/if}

      {#if shown('network') && settings}
        <section class="card stack" id="s-network">
          <div class="card-head">
            <span class="ico"><Icon name="globe" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.network')}</h2>
              <p class="small muted">{$t('settings.networkHint')}</p>
            </div>
          </div>
          <Toggle label={$t('settings.directSocket')} hint={$t('settings.directSocketHint')} bind:checked={settings.directSocket} />
          <div class="buf"><Input type="number" label={$t('settings.socketBuffer')} min="1" max="256" bind:value={settings.socketBufferMb} /></div>
          <div class="row"><Button variant="primary" on:click={save}>{$t('common.save')}</Button>{#if saved}<span class="small ok" role="status">{$t('settings.savedRestart')}</span>{/if}</div>
        </section>
      {/if}

      {#if shown('links')}
        <section class="card" id="s-links">
          <div class="card-head">
            <span class="ico"><Icon name="link" size={16} /></span>
            <div class="grow">
              <h2>{$t('links.title')}</h2>
              <p class="small muted">{$t('links.hint')}</p>
            </div>
            <Button variant="primary" on:click={() => openLinkDialog('')}>{$t('links.open')}</Button>
          </div>
        </section>
      {/if}

      {#if shown('logs')}<div id="s-logs"><Logs embedded /></div>{/if}

      {#if shown('anon')}<div id="s-anon"><AnonymityCard /></div>{/if}
      {#if shown('guard')}<div id="s-guard"><GuardCard /></div>{/if}
      {#if shown('proxy')}<div id="s-proxy"><ProxyCard /></div>{/if}
      {#if shown('protocol')}<div id="s-protocol"><ProtocolCard /></div>{/if}
      {#if shown('boot')}<div id="s-boot"><BootstrapCard /></div>{/if}

      {#if shown('lock')}
        <section class="card stack danger-card" id="s-lock">
          <div class="card-head">
            <span class="ico danger"><Icon name="lock" size={16} /></span>
            <div class="grow">
              <h2>{$t('settings.lockTitle')}</h2>
              <p class="small muted">{$t('settings.lockHint')}</p>
            </div>
          </div>
          <div class="row end">
            <div class="confirm"><Input aria-label={$t('settings.lockTitle')} bind:value={wipeText} placeholder="LOCK" autocomplete="off" /></div>
            <Button variant="danger-solid" disabled={wipeText !== 'LOCK'} on:click={wipe}>{$t('settings.lockNow')}</Button>
          </div>
        </section>
      {/if}
    </div>
  </div>
</div>

<style>
  .st { height: 100%; overflow-y: auto; padding: var(--sp-6) var(--sp-6) var(--sp-12); display: block; }
  .head { max-width: 960px; margin: 0 auto var(--sp-4); display: flex; align-items: flex-end; gap: var(--sp-3); }
  .layout { max-width: 960px; margin: 0 auto; display: grid; grid-template-columns: 208px minmax(0, 1fr); gap: var(--sp-6); align-items: start; }
  .side { position: sticky; top: 0; display: flex; flex-direction: column; gap: var(--sp-3); }
  .side nav { display: flex; flex-direction: column; gap: 2px; }
  .g {
    display: flex; align-items: center; gap: var(--sp-2); height: 32px; padding: 0 var(--sp-3); text-align: left;
    border: none; border-radius: var(--r-md); background: transparent; color: var(--muted); font-size: var(--fs-md); font-weight: var(--fw-medium);
  }
  .g:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .g.on, .g.on:hover:not(:disabled) { background: var(--accent-soft); color: var(--accent-fg); }
  .pane { display: flex; flex-direction: column; gap: var(--sp-3); min-width: 0; }
  .none { padding: var(--sp-6) 0; }
  @media (max-width: 760px) {
    .layout { grid-template-columns: minmax(0, 1fr); gap: var(--sp-3); }
    .side { position: static; }
    .side nav { flex-direction: row; flex-wrap: wrap; }
  }

  .idrow { display: flex; align-items: center; gap: var(--sp-3); flex-wrap: wrap; }
  .lbl { color: var(--muted); font-size: var(--fs-sm); font-weight: var(--fw-medium); margin-bottom: 6px; }
  .copybox { display: flex; align-items: center; gap: var(--sp-1); padding: 4px 4px 4px var(--sp-3); border-radius: var(--r-md); background: var(--surface-2); border: 1px solid var(--border); }
  .buf { max-width: 240px; }
  .confirm { width: 140px; }
  .end { align-items: flex-end; }
  .field-row { display: flex; align-items: center; gap: var(--sp-3); flex-wrap: wrap; padding-top: var(--sp-3); border-top: 1px solid var(--border); }
  .vol { color: var(--faint); }
  .vol.off { opacity: 0.5; }
  .pct { width: 40px; text-align: right; font-size: var(--fs-xs); color: var(--muted); font-variant-numeric: tabular-nums; }
  .danger-card { border-color: var(--danger-soft); }

  .themes { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: var(--sp-3); }
  .theme-opt {
    display: flex; flex-direction: column; gap: var(--sp-2); padding: var(--sp-2); border-radius: var(--r-lg); text-align: left; height: auto;
    background: var(--surface-2); border: 1px solid var(--border);
  }
  .theme-opt:hover:not(:disabled) { border-color: var(--border-strong); background: var(--surface-2); }
  .theme-opt.sel, .theme-opt.sel:hover:not(:disabled) { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
  .opt-label { gap: 6px; font-size: var(--fs-sm); color: var(--muted); padding: 0 var(--sp-1); }
  .theme-opt.sel .opt-label { color: var(--accent-fg); }
  .preview { display: flex; height: 64px; border-radius: var(--r-md); overflow: hidden; border: 1px solid var(--border); }
  .preview.dark { --p-side: #1a1e24; --p-main: #13161b; --p-in: #232932; --p-out: #25635a; }
  .preview.light { --p-side: #fbf8f3; --p-main: #f1ece4; --p-in: #ffffff; --p-out: #1a7f6e; }
  .preview.system { --p-side: linear-gradient(135deg, #fbf8f3 50%, #1a1e24 50%); --p-main: linear-gradient(135deg, #f1ece4 50%, #13161b 50%); --p-in: #8d92a0; --p-out: #4fb8a6; }
  .pv-side { width: 26%; background: var(--p-side); border-right: 1px solid rgba(128, 128, 128, 0.15); }
  .pv-main { flex: 1; background: var(--p-main); display: flex; flex-direction: column; justify-content: center; gap: 4px; padding: 8px; }
  .pv-b { height: 9px; border-radius: 5px; width: 60%; background: var(--p-in); }
  .pv-b.out { align-self: flex-end; background: var(--p-out); width: 50%; }
  .pv-b.short { width: 40%; }
</style>
