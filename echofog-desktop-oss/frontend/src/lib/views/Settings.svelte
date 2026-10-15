<script>
  import { onMount } from 'svelte';
  import { App, errText, copy } from '../api.js';
  import { me, profile, logout } from '../stores/session.js';
  import { closeChats } from '../stores/chats.js';
  import { panicClear, endCall } from '../stores/call.store.js';
  import { soundSettings, play } from '../sounds.js';
  import { displayName } from '../contact.js';
  import { theme } from '../stores/theme.js';
  import Icon from '../components/ui/Icon.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import BootstrapCard from '../components/BootstrapCard.svelte';
  import { t, locale, LANGUAGES } from '../i18n/index.js';

  const THEMES = [
    { id: 'system', label: 'settings.themeSystem', icon: 'monitor' },
    { id: 'light', label: 'settings.themeLight', icon: 'sun' },
    { id: 'dark', label: 'settings.themeDark', icon: 'moon' },
  ];

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
    <div class="page-head">
      <div class="grow">
        <h1>{$t('settings.title')}</h1>
        <p class="muted small">{$t('settings.subtitle')}</p>
      </div>
    </div>
    {#if error}<p class="error small">{error}</p>{/if}

    <div class="card stack">
      <div class="idrow">
        <Avatar id={$profile} name={($displayName || '').trim() || $profile} size={60} />
        <div class="grow">
          <h2>{($displayName || '').trim() || $profile}</h2>
          <p class="small muted">{$t('settings.profileNamed', { name: $profile })}</p>
        </div>
        <button class="with-icon" on:click={signOut}><Icon name="logout" size={16} /> {$t('settings.signOut')}</button>
      </div>
      <label>{$t('settings.displayName')}
        <input bind:value={$displayName} placeholder={$profile} maxlength="64" />
      </label>
      <div>
        <div class="lbl">{$t('settings.yourKey')}</div>
        <div class="copybox">
          <span class="mono grow">{$me.pubKey}</span>
          <button class="icon" title={$t('settings.copyKey')} on:click={copyKey}><Icon name={copied ? 'check' : 'copy'} size={16} /></button>
        </div>
      </div>
    </div>

    <div class="card stack">
      <div class="card-head">
        <span class="ico"><Icon name="palette" size={19} /></span>
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
            <span class="row opt-label"><Icon name={th.icon} size={15} /> {$t(th.label)}</span>
          </button>
        {/each}
      </div>
    </div>

    <div class="card stack">
      <div class="card-head">
        <span class="ico"><Icon name="globe" size={19} /></span>
        <div class="grow">
          <h2>{$t('settings.language')}</h2>
          <p class="small muted">{$t('settings.languageHint')}</p>
        </div>
      </div>
      <div class="seg" role="radiogroup" aria-label={$t('settings.language')}>
        {#each [{ id: 'system', label: $t('settings.langSystem') }, ...LANGUAGES] as l}
          <button role="radio" class:active={$locale === l.id} aria-checked={$locale === l.id} lang={l.id === 'system' ? undefined : l.id} on:click={() => locale.set(l.id)}>{l.label}</button>
        {/each}
      </div>
    </div>

    <div class="card stack">
      <div class="card-head">
        <span class="ico"><Icon name="bell" size={19} /></span>
        <div class="grow">
          <h2>{$t('settings.sounds')}</h2>
          <p class="small muted">{$t('settings.soundsHint')}</p>
        </div>
        <input type="checkbox" class="switch" aria-label={$t('settings.sounds')} bind:checked={$soundSettings.enabled} />
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
          <button class="small" data-silent disabled={!$soundSettings.enabled} on:click={() => play(name)}>{$t(title)}</button>
        {/each}
      </div>
    </div>

    {#if settings}
      <div class="card stack">
        <div class="card-head">
          <span class="ico"><Icon name="globe" size={19} /></span>
          <div class="grow">
            <h2>{$t('settings.network')}</h2>
            <p class="small muted">{$t('settings.networkHint')}</p>
          </div>
        </div>
        <label class="row opt">
          <span class="grow">
            {$t('settings.directSocket')}
            <span class="small muted block">{$t('settings.directSocketHint')}</span>
          </span>
          <input type="checkbox" class="switch" bind:checked={settings.directSocket} />
        </label>
        <label class="buf">{$t('settings.socketBuffer')}
          <input type="number" min="1" max="256" bind:value={settings.socketBufferMb} />
        </label>
        <div class="row"><button class="primary" on:click={save}>{$t('common.save')}</button>{#if saved}<span class="small ok saved">{$t('settings.savedRestart')}</span>{/if}</div>
      </div>
    {/if}

    <BootstrapCard />

    <div class="card stack danger-card">
      <div class="card-head">
        <span class="ico danger"><Icon name="lock" size={19} /></span>
        <div class="grow">
          <h2>{$t('settings.lockTitle')}</h2>
          <p class="small muted">
            {$t('settings.lockHint')}
          </p>
        </div>
      </div>
      <div class="row">
        <input class="confirm" bind:value={wipeText} placeholder="LOCK" />
        <button class="danger solid" disabled={wipeText !== 'LOCK'} on:click={wipe}>{$t('settings.lockNow')}</button>
      </div>
    </div>
  </div>
</div>

<style>
  .idrow { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; }
  .lbl { color: var(--muted); font-size: 12.5px; font-weight: 500; margin-bottom: 6px; }
  .copybox { display: flex; align-items: center; gap: 6px; padding: 8px 6px 8px 12px; border-radius: 12px; background: var(--raised); }
  .buf { max-width: 280px; }
  .confirm { width: 130px; font-family: var(--mono); letter-spacing: 0.1em; }
  .block { display: block; margin-top: 2px; }
  .opt { align-items: center; }
  .saved { animation: fade-up 0.3s var(--ease); }
  .vol { color: var(--faint); transition: opacity 0.2s; }
  .vol.off { opacity: 0.5; }
  .pct { width: 40px; text-align: right; font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
  .danger-card { border-color: color-mix(in srgb, var(--danger) 22%, transparent); }

  .themes { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; }
  .theme-opt {
    display: flex; flex-direction: column; gap: 10px; padding: 10px; border-radius: 16px; text-align: left;
    background: var(--raised); border: 2px solid transparent;
  }
  .theme-opt:hover:not(:disabled) { border-color: var(--line); background: var(--raised); transform: translateY(-2px); }
  .theme-opt.sel, .theme-opt.sel:hover:not(:disabled) { border-color: var(--accent); box-shadow: 0 0 0 4px var(--accent-soft); }
  .opt-label { gap: 7px; font-size: 13px; color: var(--muted); padding: 0 4px; }
  .theme-opt.sel .opt-label { color: var(--accent); }
  .preview { display: flex; height: 76px; border-radius: 10px; overflow: hidden; border: 1px solid var(--line-soft); }
  .preview.dark { --p-side: #121319; --p-main: #0b0c10; --p-in: #1d1f27; }
  .preview.light { --p-side: #ffffff; --p-main: #f3f4f8; --p-in: #ffffff; }
  .preview.system { --p-side: linear-gradient(135deg, #ffffff 50%, #121319 50%); --p-main: linear-gradient(135deg, #f3f4f8 50%, #0b0c10 50%); --p-in: #8d92a0; }
  .pv-side { width: 26%; background: var(--p-side); border-right: 1px solid rgba(128, 128, 128, 0.15); }
  .pv-main { flex: 1; background: var(--p-main); display: flex; flex-direction: column; justify-content: center; gap: 5px; padding: 8px; }
  .pv-b { height: 10px; border-radius: 5px; width: 60%; background: var(--p-in); box-shadow: 0 1px 2px rgba(0, 0, 0, 0.12); }
  .pv-b.out { align-self: flex-end; background: linear-gradient(135deg, #5b8cff, #7a62ff); width: 50%; }
  .pv-b.short { width: 40%; }
</style>

