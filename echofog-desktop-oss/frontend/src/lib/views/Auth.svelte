<script>
  import {
    screen, profiles, profile, recoveryPhrase, unlockNote,
    choose, login, create, restore, phraseSaved, chooseAccess, removeProfile, errText,
  } from '../stores/session.js';
  import { App } from '../api.js';
  import { resolvedTheme, toggleTheme } from '../stores/theme.js';
  import Icon from '../components/ui/Icon.svelte';
  import Avatar from '../components/ui/Avatar.svelte';
  import Button from '../components/ui/Button.svelte';
  import IconButton from '../components/ui/IconButton.svelte';
  import Input from '../components/ui/Input.svelte';
  import Textarea from '../components/ui/Textarea.svelte';
  import Tabs from '../components/ui/Tabs.svelte';
  import Checkbox from '../components/ui/Checkbox.svelte';
  import Badge from '../components/ui/Badge.svelte';
  import { t, lang, locale, LANGUAGES } from '../i18n/index.js';

  let password = '';
  let confirm = '';
  let phrase = '';
  let newName = '';
  let saved = false;
  let busy = false;
  let error = '';
  let passphrase = '';
  let phraseCheck = ''; // what is wrong with the typed phrase, '' when it is fine
  let phraseOk = false;
  let checkTimer;
  let accessMode = 'public';
  let rememberMe = false;
  let rememberOffer = false; // this system can remember the unlock, and this profile's is not

  $: if ($screen === 'login') offerRemember();
  async function offerRemember() {
    try {
      const st = await App.GetRememberedUnlock();
      rememberOffer = st.supported && !st.enabled;
    } catch (_) {
      rememberOffer = false;
    }
  }

  // the kill switch held the computer's traffic after EchoFog went away: say so before login
  let held = false;
  App.KillSwitchHeld().then((h) => (held = h)).catch(() => {});
  async function unblock() {
    try { await App.ReleaseKillSwitch(); held = false; } catch (err) { error = errText(err); }
  }

  // the words are checked while they are typed, against the same list the core uses
  $: if ($screen === 'restore') checkPhrase(phrase);
  function checkPhrase(p) {
    clearTimeout(checkTimer);
    phraseOk = false;
    const words = p.trim().split(/\s+/).filter(Boolean);
    if (!words.length) { phraseCheck = ''; return; }
    if (words.length < 12) { phraseCheck = $t('auth.wordsCount', { n: words.length }); return; }
    checkTimer = setTimeout(async () => {
      try {
        phraseCheck = (await App.CheckRecoveryPhrase(p)) || '';
        phraseOk = !phraseCheck;
      } catch (_) {
        phraseCheck = '';
      }
    }, 250);
  }

  function reset() {
    password = confirm = phrase = passphrase = '';
    phraseCheck = '';
    phraseOk = false;
    saved = false;
    rememberMe = false;
    error = '';
    unlockNote.set(null);
  }

  async function run(fn) {
    busy = true;
    error = '';
    try {
      await fn();
    } catch (err) {
      error = errText(err);
    } finally {
      busy = false;
    }
  }

  const go = (s) => { reset(); screen.set(s); };

  // the create / restore switch is a Tabs control: it follows the screen and changes it
  let tab = 'create';
  $: if ($screen === 'create' || $screen === 'restore') tab = $screen;
  const onTab = (e) => go(e.detail);

  // focus the first text field of a step when it appears
  function focusFirst(node) {
    const id = setTimeout(() => node.querySelector('input:not([type=checkbox]), textarea')?.focus(), 30);
    return { destroy: () => clearTimeout(id) };
  }

  $: strength = password.length >= 12 ? 'good' : password.length >= 8 ? 'ok' : password.length ? 'weak' : '';
  $: phraseWords = phrase.trim().split(/\s+/).filter(Boolean).length;

  function submitCreate() {
    if (password.length < 8) return (error = $t('auth.tooShort'));
    if (password !== confirm) return (error = $t('auth.mismatch'));
    run(() => create(password));
  }

  async function submitRestore() {
    const problem = await App.CheckRecoveryPhrase(phrase);
    if (problem) return (error = problem);
    if (password.length < 8) return (error = $t('auth.tooShort'));
    if (password !== confirm) return (error = $t('auth.mismatch'));
    run(() => restore(phrase, password, passphrase));
  }

  let delName = '';
  let delPassword = '';

  function askDelete(name) {
    delName = delName === name ? '' : name;
    delPassword = '';
    error = '';
  }
  function cancelDelete() {
    delName = '';
    delPassword = '';
  }
  async function confirmDelete() {
    if (!delPassword) { error = $t('auth.passwordRequired'); return; }
    const name = delName;
    await run(() => removeProfile(name, delPassword));
    if (!error) { delName = ''; delPassword = ''; }
  }
</script>

<div class="wrap">
  <div class="top left" role="group" aria-label={$t('auth.language')}>
    {#each LANGUAGES as l}
      <button class="lang" class:on={$lang === l.id} aria-pressed={$lang === l.id} title={l.label} on:click={() => locale.set(l.id)}>{l.id.toUpperCase()}</button>
    {/each}
  </div>

  <div class="top right">
    <IconButton icon={$resolvedTheme === 'dark' ? 'sun' : 'moon'} label={$resolvedTheme === 'dark' ? $t('auth.lightTheme') : $t('auth.darkTheme')} on:click={toggleTheme} />
  </div>

  <main class="box">
    <header class="hero">
      <span class="mark" aria-hidden="true"><Icon name="signal" size={20} stroke={2} /></span>
      <div>
        <h1>EchoFog</h1>
        <p class="muted">{$t('auth.tagline')}</p>
      </div>
    </header>

    {#if held}
      <div class="notice warn" role="alert">
        <Icon name="warning" size={16} />
        <span class="grow">{$t('proxy.killHeld')}</span>
        <Button size="sm" on:click={unblock}>{$t('proxy.killUnblock')}</Button>
      </div>
    {/if}

    {#key $screen}
    <div class="step" use:focusFirst>
    {#if $screen === 'profiles'}
      <h2>{$t('auth.chooseProfile')}</h2>
      <ul class="profiles">
        {#each $profiles as name}
          <li>
            <div class="prof">
              <button class="pick" on:click={() => run(() => choose(name))} disabled={busy}>
                <Avatar id={name} {name} size={32} />
                <span class="grow nm">{name}</span>
                <Icon name="chevronRight" size={16} />
              </button>
              <IconButton icon="trash" label={$t('auth.deleteProfileNamed', { name })} pressed={delName === name} disabled={busy} on:click={() => askDelete(name)} />
            </div>
            {#if delName === name}
              <form class="del-confirm" on:submit|preventDefault={confirmDelete}>
                <p class="small muted">{$t('auth.confirmDelete', { name })}</p>
                <p class="small faint">{$t('auth.deletePasswordHint')}</p>
                <Input type="password" label={$t('auth.password')} bind:value={delPassword} autocomplete="off" />
                <div class="row">
                  <Button className="grow" on:click={cancelDelete} disabled={busy}>{$t('common.cancel')}</Button>
                  <Button className="grow" variant="danger-solid" type="submit" disabled={busy || !delPassword}>{$t('auth.delete')}</Button>
                </div>
              </form>
            {/if}
          </li>
        {/each}
      </ul>
      <form class="row add" on:submit|preventDefault={() => run(() => choose(newName))}>
        <div class="grow"><Input placeholder={$t('auth.newProfileName')} aria-label={$t('auth.newProfileName')} bind:value={newName} /></div>
        <Button variant="primary" type="submit" disabled={busy || !newName.trim()}>{$t('common.create')}</Button>
      </form>

    {:else if $screen === 'login'}
      <div class="who">
        <Avatar id={$profile} name={$profile} size={56} />
        <h2>{$t('auth.welcomeBack', { name: $profile })}</h2>
      </div>
      {#if $unlockNote}<p class="small warn" role="status">{$t('remember.reason.' + $unlockNote.reason, { error: $unlockNote.error })}</p>{/if}
      <form class="stack" on:submit|preventDefault={() => run(() => login(password, rememberMe))}>
        <Input type="password" label={$t('auth.password')} bind:value={password} />
        {#if rememberOffer}
          <Checkbox bind:checked={rememberMe} label={$t('remember.loginCheckbox')} hint={rememberMe ? $t('remember.loginHint') : ''} />
        {/if}
        <Button variant="primary" size="lg" block type="submit" icon="lockOpen" loading={busy} disabled={!password}>{busy ? $t('auth.unlocking') : $t('auth.unlock')}</Button>
      </form>
      <div class="row links">
        <Button variant="ghost" size="sm" on:click={() => go('profiles')}>{$t('auth.otherProfile')}</Button>
        <span class="grow"></span>
        <Button variant="ghost" size="sm" on:click={() => go('restore')}>{$t('auth.forgotPassword')}</Button>
      </div>

    {:else if $screen === 'create' || $screen === 'restore'}
      <Tabs variant="segment" label={$t('auth.newOrExisting')} bind:value={tab} on:change={onTab}
        items={[{ id: 'create', label: $t('auth.newIdentity') }, { id: 'restore', label: $t('auth.recoveryPhrase') }]} />
    {/if}

    {#if $screen === 'create'}
      <div>
        <h2>{$t('auth.createTitle')}</h2>
        <p class="muted small lead">{$t('auth.createHint')}</p>
      </div>
      <form class="stack" on:submit|preventDefault={submitCreate}>
        <Input type="password" label={$t('auth.password')} bind:value={password} />
        <div class="meter {strength}" aria-hidden="true"><span></span><span></span><span></span></div>
        <Input type="password" label={$t('auth.repeatPassword')} bind:value={confirm} />
        <Button variant="primary" size="lg" block type="submit" loading={busy}>{busy ? $t('auth.creating') : $t('common.create')}</Button>
      </form>
      {#if $profiles.length}<div class="row"><Button variant="ghost" size="sm" icon="arrowLeft" on:click={() => go('profiles')}>{$t('common.back')}</Button></div>{/if}

    {:else if $screen === 'restore'}
      <div>
        <h2>{$t('auth.restoreTitle')}</h2>
        <p class="muted small lead">{$t('auth.restoreHint')}</p>
      </div>
      <form class="stack" on:submit|preventDefault={submitRestore}>
        <Textarea rows={3} label={$t('auth.recoveryPhrase')} bind:value={phrase} spellcheck="false" autocomplete="off" placeholder={$t('auth.phrasePlaceholder')} />
        {#if phraseCheck}<p class="small" class:error={phraseWords >= 12} class:faint={phraseWords < 12}>{phraseCheck}</p>
        {:else if phraseOk}<p class="small ok">{$t('auth.phraseValid')}</p>{/if}
        <details>
          <summary class="small muted">{$t('auth.hasPassphrase')}</summary>
          <div class="pp"><Input type="password" label={$t('auth.extraPassphrase')} bind:value={passphrase} autocomplete="off" /></div>
        </details>
        <Input type="password" label={$t('auth.newPassword')} bind:value={password} />
        <Input type="password" label={$t('auth.repeatPassword')} bind:value={confirm} />
        <Button variant="primary" size="lg" block type="submit" loading={busy}>{busy ? $t('auth.restoring') : $t('auth.restore')}</Button>
      </form>
      {#if $profiles.length}<div class="row"><Button variant="ghost" size="sm" icon="arrowLeft" on:click={() => go('profiles')}>{$t('common.back')}</Button></div>{/if}

    {:else if $screen === 'phrase'}
      <div>
        <h2>{$t('auth.phraseTitle')}</h2>
        <p class="muted small lead">{$t('auth.phraseHint')}</p>
      </div>
      <ol class="words">
        {#each $recoveryPhrase.split(' ') as w, i}<li><span class="n">{i + 1}</span><span class="mono">{w}</span></li>{/each}
      </ol>
      <Checkbox bind:checked={saved} label={$t('auth.phraseSaved')} />
      <Button variant="primary" size="lg" block disabled={!saved} loading={busy} on:click={() => run(phraseSaved)}>{$t('auth.continue')}</Button>

    {:else if $screen === 'access'}
      <h2>{$t('auth.access.title')}</h2>
      <div class="modes" role="radiogroup" aria-label={$t('auth.access.title')}>
        {#each ['public', 'hidden'] as m}
          <button class="mode" class:sel={accessMode === m} role="radio" aria-checked={accessMode === m} on:click={() => (accessMode = m)}>
            <span class="mode-ico"><Icon name={m === 'public' ? 'globe' : 'lock'} size={18} /></span>
            <span class="mode-text">
              <b>{$t('auth.access.' + m + 'Title')}{#if m === 'public'} <Badge variant="accent" size="sm">{$t('auth.access.recommended')}</Badge>{/if}</b>
              <span class="small muted">{$t('auth.access.' + m + 'Text')}</span>
            </span>
            <span class="radio" aria-hidden="true"></span>
          </button>
        {/each}
      </div>
      <p class="small faint">{$t('auth.access.hint')}</p>
      <Button variant="primary" size="lg" block loading={busy} on:click={() => run(() => chooseAccess(accessMode))}>{$t('auth.access.start')}</Button>
    {/if}
    </div>
    {/key}

    {#if error}<p class="notice err" role="alert"><Icon name="error" size={16} /> <span>{error}</span></p>{/if}
  </main>
</div>

<style>
  .wrap {
    position: relative; height: 100%; display: grid; place-items: center; overflow-y: auto; overflow-x: hidden;
    padding: var(--sp-6) var(--sp-4); background: var(--bg);
  }
  .top { position: fixed; top: var(--sp-4); z-index: 2; display: flex; gap: 2px; }
  .top.left { left: var(--sp-4); padding: 2px; border-radius: var(--r-md); background: var(--surface-1); border: 1px solid var(--border); }
  .top.right { right: var(--sp-4); }
  .lang { height: 24px; padding: 0 8px; font-size: var(--fs-xs); font-weight: var(--fw-semibold); border: none; border-radius: var(--r-sm); background: transparent; color: var(--muted); }
  .lang:hover:not(:disabled) { background: var(--hover); color: var(--text); border: none; }
  .lang.on, .lang.on:hover:not(:disabled) { background: var(--accent-soft); color: var(--accent-fg); }

  .box {
    width: min(400px, 100%); padding: var(--sp-8); border-radius: var(--r-xl); display: flex; flex-direction: column; gap: var(--sp-5);
    background: var(--surface-1); border: 1px solid var(--border); box-shadow: var(--shadow-md);
  }
  .step { display: flex; flex-direction: column; gap: var(--sp-4); }
  .hero { display: flex; gap: var(--sp-3); align-items: center; }
  .hero h1 { font-size: var(--fs-xl); line-height: var(--lh-xl); }
  .hero p { font-size: var(--fs-sm); margin-top: 2px; }
  .mark { width: 40px; height: 40px; flex: none; border-radius: var(--r-lg); display: grid; place-items: center; background: var(--accent); color: var(--accent-ink); }
  .lead { margin-top: var(--sp-1); line-height: 1.5; }

  .profiles { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: var(--sp-1); }
  .prof { display: flex; gap: var(--sp-1); align-items: center; }
  .pick {
    flex: 1; min-width: 0; display: flex; align-items: center; gap: var(--sp-3); text-align: left; height: 48px;
    padding: 0 var(--sp-3) 0 var(--sp-2); border-radius: var(--r-md); color: var(--text); background: var(--surface-2); border: 1px solid var(--border);
  }
  .pick :global(svg:last-child) { color: var(--faint); }
  .pick:hover:not(:disabled) :global(svg:last-child) { color: var(--accent-fg); }
  .nm { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: var(--fw-medium); }
  .del-confirm { display: flex; flex-direction: column; gap: var(--sp-3); padding: var(--sp-3); margin: var(--sp-1) 0; border-radius: var(--r-md); background: var(--surface-2); border: 1px solid var(--border); }
  .add { align-items: flex-end; }

  .who { display: flex; flex-direction: column; align-items: center; gap: var(--sp-2); text-align: center; }
  .links { margin-top: calc(var(--sp-1) * -1); }
  .pp { margin-top: var(--sp-2); }
  .meter { display: grid; grid-template-columns: repeat(3, 1fr); gap: 4px; margin-top: -6px; }
  .meter span { height: 3px; border-radius: 2px; background: var(--surface-3); transition: background var(--dur) var(--ease); }
  .meter.weak span:nth-child(1) { background: var(--danger); }
  .meter.ok span:nth-child(-n + 2) { background: var(--warn); }
  .meter.good span { background: var(--ok); }

  .words { display: grid; grid-template-columns: repeat(3, 1fr); gap: var(--sp-1); margin: 0; padding: 0; list-style: none; }
  .words li { display: flex; align-items: center; gap: var(--sp-2); padding: 6px 10px; border-radius: var(--r-sm); background: var(--surface-2); border: 1px solid var(--border); }
  .words .n { color: var(--faint); font-size: var(--fs-2xs); width: 16px; text-align: right; font-variant-numeric: tabular-nums; }
  .words .mono { color: var(--text); font-size: var(--fs-sm); word-break: normal; }

  .modes { display: flex; flex-direction: column; gap: var(--sp-2); }
  .mode {
    display: flex; gap: var(--sp-3); align-items: flex-start; text-align: left; padding: var(--sp-3); border-radius: var(--r-lg);
    background: var(--surface-2); border: 1px solid var(--border); color: var(--text); white-space: normal; height: auto; width: 100%; font-weight: var(--fw-regular);
  }
  .mode.sel, .mode.sel:hover:not(:disabled) { border-color: var(--accent); background: var(--accent-soft); }
  .mode-ico { width: 32px; height: 32px; flex: none; border-radius: var(--r-md); display: grid; place-items: center; background: var(--surface-1); color: var(--accent-fg); }
  .mode-text { display: flex; flex-direction: column; gap: 3px; flex: 1; min-width: 0; }
  .mode-text b { font-weight: var(--fw-semibold); }
  .radio { width: 16px; height: 16px; flex: none; margin-top: 2px; border-radius: 50%; border: 2px solid var(--border-strong); }
  .mode.sel .radio { border-color: var(--accent); background: radial-gradient(var(--accent) 45%, transparent 50%); }

  .notice { display: flex; align-items: flex-start; gap: var(--sp-2); padding: var(--sp-2) var(--sp-3); border-radius: var(--r-md); font-size: var(--fs-sm); }
  .notice.warn { background: var(--warn-soft); color: var(--warn-fg); align-items: center; }
  .notice.err { background: var(--danger-soft); color: var(--danger-fg); }
</style>
