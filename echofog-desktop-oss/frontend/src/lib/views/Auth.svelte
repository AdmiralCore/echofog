<script>
  import {
    screen, profiles, profile, recoveryPhrase,
    choose, login, create, restore, phraseSaved, removeProfile, errText,
  } from '../stores/session.js';
  import { App } from '../api.js';
  import { resolvedTheme, toggleTheme } from '../stores/theme.js';
  import Icon from '../components/ui/Icon.svelte';
  import Avatar from '../components/ui/Avatar.svelte';

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

  // the words are checked while they are typed, against the same list the core uses
  $: if ($screen === 'restore') checkPhrase(phrase);
  function checkPhrase(p) {
    clearTimeout(checkTimer);
    phraseOk = false;
    const words = p.trim().split(/\s+/).filter(Boolean);
    if (!words.length) { phraseCheck = ''; return; }
    if (words.length < 12) { phraseCheck = `${words.length} of 12 or 24 words`; return; }
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
    error = '';
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

  function submitCreate() {
    if (password.length < 8) return (error = 'Use at least 8 characters.');
    if (password !== confirm) return (error = 'The passwords do not match.');
    run(() => create(password));
  }

  async function submitRestore() {
    const problem = await App.CheckRecoveryPhrase(phrase);
    if (problem) return (error = problem);
    if (password.length < 8) return (error = 'Use at least 8 characters.');
    if (password !== confirm) return (error = 'The passwords do not match.');
    run(() => restore(phrase, password, passphrase));
  }

  async function del(name) {
    if (!confirm_(`Delete profile "${name}" and all its data from this computer? This cannot be undone.`)) return;
    run(() => removeProfile(name));
  }
  const confirm_ = (t) => window.confirm(t);
</script>

<div class="wrap">
  <div class="orb o1" aria-hidden="true"></div>
  <div class="orb o2" aria-hidden="true"></div>
  <div class="orb o3" aria-hidden="true"></div>

  <button class="icon theme" title={$resolvedTheme === 'dark' ? 'Light theme' : 'Dark theme'} aria-label="Toggle theme" on:click={toggleTheme}>
    <Icon name={$resolvedTheme === 'dark' ? 'sun' : 'moon'} size={18} />
  </button>

  <div class="box stack">
    <div class="hero">
      <span class="mark" aria-hidden="true"><span></span></span>
      <div>
        <h1>EchoFog</h1>
        <p class="muted small">Messages, calls and proxy without a central server.</p>
      </div>
    </div>

    {#key $screen}
    <div class="step stack">
    {#if $screen === 'profiles'}
      <h2>Choose a profile</h2>
      <div class="profiles">
        {#each $profiles as name}
          <div class="prof">
            <button class="pick" on:click={() => run(() => choose(name))} disabled={busy}>
              <Avatar id={name} {name} size={36} />
              <span class="grow">{name}</span>
              <Icon name="chevronRight" size={16} />
            </button>
            <button class="icon del" title="Delete profile" aria-label="Delete profile {name}" on:click={() => del(name)} disabled={busy}><Icon name="trash" size={16} /></button>
          </div>
        {/each}
      </div>
      <form class="row" on:submit|preventDefault={() => run(() => choose(newName))}>
        <input class="grow" placeholder="New profile name" bind:value={newName} />
        <button type="submit" class="primary" disabled={busy || !newName.trim()}>Create</button>
      </form>

    {:else if $screen === 'login'}
      <div class="who">
        <Avatar id={$profile} name={$profile} size={56} />
        <h2>Welcome back, {$profile}</h2>
      </div>
      <form class="stack" on:submit|preventDefault={() => run(() => login(password))}>
        <label>Password<input type="password" bind:value={password} autofocus /></label>
        <button class="primary big" type="submit" disabled={busy || !password}>{#if busy}<span class="spinner light"></span> Unlocking…{:else}<Icon name="lockOpen" size={16} /> Unlock{/if}</button>
      </form>
      <div class="row links">
        <button class="ghost small" on:click={() => go('profiles')}>Other profile</button>
        <span class="grow"></span>
        <button class="ghost small" on:click={() => go('restore')}>Forgot password</button>
      </div>

    {:else if $screen === 'create' || $screen === 'restore'}
      <div class="seg" role="tablist" aria-label="New or existing identity">
        <button role="tab" class:active={$screen === 'create'} aria-selected={$screen === 'create'} on:click={() => go('create')}>New identity</button>
        <button role="tab" class:active={$screen === 'restore'} aria-selected={$screen === 'restore'} on:click={() => go('restore')}>Recovery phrase</button>
      </div>
    {/if}

    {#if $screen === 'create'}
      <h2>Create an identity</h2>
      <p class="muted small">
        The password encrypts your keys on this computer. It cannot be reset; the recovery phrase shown next is the only backup.
      </p>
      <form class="stack" on:submit|preventDefault={submitCreate}>
        <label>Password<input type="password" bind:value={password} autofocus /></label>
        <div class="meter" aria-hidden="true"><span style="width:{Math.min(100, password.length * 8)}%" class:good={password.length >= 12} class:ok={password.length >= 8 && password.length < 12}></span></div>
        <label>Repeat password<input type="password" bind:value={confirm} /></label>
        <button class="primary big" type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create'}</button>
      </form>
      {#if $profiles.length}<div class="row"><button class="ghost small with-icon" on:click={() => go('profiles')}><Icon name="arrowLeft" size={15} /> Back</button></div>{/if}

    {:else if $screen === 'restore'}
      <h2>Restore from recovery phrase</h2>
      <p class="muted small">Type or paste the 12 or 24 words in their order. The same phrase restores the same identity and wallet as in the app or on a server node.</p>
      <form class="stack" on:submit|preventDefault={submitRestore}>
        <label>Recovery phrase<textarea rows="3" bind:value={phrase} spellcheck="false" autocomplete="off" placeholder="word word word …" /></label>
        {#if phraseCheck}<p class="small" class:error={phrase.trim().split(/\s+/).length >= 12} class:faint={phrase.trim().split(/\s+/).length < 12}>{phraseCheck}</p>
        {:else if phraseOk}<p class="small ok">✓ The phrase is valid.</p>{/if}
        <details>
          <summary class="small muted">The phrase was made with an extra passphrase</summary>
          <label class="pp">Extra passphrase<input type="password" bind:value={passphrase} autocomplete="off" /></label>
        </details>
        <label>New password<input type="password" bind:value={password} /></label>
        <label>Repeat password<input type="password" bind:value={confirm} /></label>
        <button class="primary big" type="submit" disabled={busy}>{busy ? 'Restoring…' : 'Restore'}</button>
      </form>
      {#if $profiles.length}<div class="row"><button class="ghost small with-icon" on:click={() => go('profiles')}><Icon name="arrowLeft" size={15} /> Back</button></div>{/if}

    {:else if $screen === 'phrase'}
      <h2>Your recovery phrase</h2>
      <p class="muted small">
        Write these words down in order and keep them offline. Anyone with them controls your identity and wallet.
        They are shown only once.
      </p>
      <ol class="words">
        {#each $recoveryPhrase.split(' ') as w, i}<li style="animation-delay:{i * 18}ms"><span class="n">{i + 1}</span><span class="mono">{w}</span></li>{/each}
      </ol>
      <label class="row"><input type="checkbox" bind:checked={saved} /> I have written the phrase down</label>
      <button class="primary big" disabled={!saved || busy} on:click={() => run(phraseSaved)}>Continue</button>
    {/if}
    </div>
    {/key}

    {#if error}<p class="error small err">{error}</p>{/if}
  </div>
</div>

<style>
  .wrap {
    position: relative; height: 100%; display: grid; place-items: center; overflow-y: auto; overflow-x: hidden; padding: 24px 16px;
    background: var(--bg);
  }
  .orb { position: absolute; border-radius: 50%; filter: blur(70px); opacity: 0.55; pointer-events: none; animation: drift 18s ease-in-out infinite; }
  .o1 { width: 420px; height: 420px; left: -120px; top: -120px; background: var(--accent); opacity: 0.22; }
  .o2 { width: 380px; height: 380px; right: -100px; bottom: -120px; background: var(--accent-2); opacity: 0.2; animation-delay: -6s; }
  .o3 { width: 240px; height: 240px; right: 18%; top: 12%; background: var(--ok); opacity: 0.08; animation-delay: -12s; }
  @keyframes drift { 0%, 100% { transform: translate(0, 0) scale(1); } 33% { transform: translate(40px, 30px) scale(1.08); } 66% { transform: translate(-30px, 20px) scale(0.95); } }
  .theme { position: absolute; top: 16px; right: 16px; z-index: 2; background: var(--glass); backdrop-filter: blur(10px); border: 1px solid var(--line-soft); }

  .box {
    position: relative; z-index: 1; width: min(420px, 100%); padding: 30px; border-radius: var(--radius-xl);
    background: var(--glass); border: 1px solid var(--line-soft);
    box-shadow: var(--shadow-lg); backdrop-filter: blur(24px) saturate(1.4);
    animation: rise 0.5s var(--spring);
  }
  .step { animation: fade-up 0.32s var(--ease); }
  .hero { display: flex; gap: 14px; align-items: center; margin-bottom: 6px; }
  .mark {
    width: 48px; height: 48px; border-radius: 15px; flex: none; position: relative; overflow: hidden;
    background: linear-gradient(135deg, var(--accent), var(--accent-2)); box-shadow: var(--glow);
  }
  .mark span { position: absolute; inset: 11px; border-radius: 50%; border: 3.5px solid rgba(255, 255, 255, 0.95); border-right-color: transparent; transform: rotate(-30deg); animation: spin 6s linear infinite; }
  .mark::after { content: ''; position: absolute; width: 9px; height: 9px; border-radius: 50%; background: #fff; left: 19.5px; top: 19.5px; }

  .profiles { display: flex; flex-direction: column; gap: 6px; }
  .prof { display: flex; gap: 6px; align-items: center; }
  .pick { flex: 1; display: flex; align-items: center; gap: 12px; text-align: left; padding: 8px 12px 8px 8px; border-radius: 14px; color: var(--text); }
  .pick :global(svg:last-child) { color: var(--faint); transition: transform 0.3s var(--spring); }
  .pick:hover:not(:disabled) :global(svg:last-child) { transform: translateX(3px); color: var(--accent); }
  .del:hover:not(:disabled) { color: var(--danger); background: var(--danger-soft); }

  .who { display: flex; flex-direction: column; align-items: center; gap: 10px; text-align: center; padding: 4px 0; }
  .big { height: 44px; display: inline-flex; align-items: center; justify-content: center; gap: 8px; font-size: 14.5px; }
  .spinner.light { border-color: rgba(255, 255, 255, 0.35); border-top-color: #fff; }
  .links { margin-top: -4px; }
  .seg { align-self: stretch; }
  .pp { margin-top: 8px; }
  .meter { height: 4px; border-radius: 4px; background: var(--raised); overflow: hidden; margin-top: -6px; }
  .meter span { display: block; height: 100%; background: var(--danger); border-radius: 4px; transition: width 0.3s var(--ease), background 0.3s; }
  .meter span.ok { background: var(--warn); }
  .meter span.good { background: var(--ok); }

  .words {
    display: grid; grid-template-columns: repeat(auto-fill, minmax(110px, 1fr)); gap: 6px;
    margin: 0; padding: 0; list-style: none;
  }
  .words li {
    display: flex; align-items: center; gap: 8px; padding: 7px 10px; border-radius: 10px;
    background: var(--raised); border: 1px solid var(--line-soft); animation: pop 0.35s var(--spring) both;
  }
  .words .n { color: var(--faint); font-size: 11px; width: 16px; text-align: right; font-variant-numeric: tabular-nums; }
  .words .mono { color: var(--text); font-size: 13px; word-break: normal; }
  .err { animation: shake 0.4s var(--ease); }
  @keyframes shake { 0%, 100% { transform: none; } 25% { transform: translateX(-5px); } 50% { transform: translateX(4px); } 75% { transform: translateX(-2px); } }
</style>

