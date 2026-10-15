<script>
  import {
    screen, profiles, profile, recoveryPhrase,
    choose, login, create, restore, phraseSaved, removeProfile, errText,
  } from '../stores/session.js';
  import { App } from '../api.js';

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
  <div class="box stack">
    <div class="hero">
      <span class="mark" aria-hidden="true"></span>
      <div>
        <h1>EchoFog</h1>
        <p class="muted small">Messages, calls and proxy without a central server.</p>
      </div>
    </div>

    {#if $screen === 'profiles'}
      <h2>Choose a profile</h2>
      <div class="stack tight">
        {#each $profiles as name}
          <div class="row">
            <button class="grow left" on:click={() => run(() => choose(name))} disabled={busy}>{name}</button>
            <button class="ghost danger" on:click={() => del(name)} disabled={busy}>Delete</button>
          </div>
        {/each}
      </div>
      <form class="row" on:submit|preventDefault={() => run(() => choose(newName))}>
        <input class="grow" placeholder="New profile name" bind:value={newName} />
        <button type="submit" disabled={busy || !newName.trim()}>Create</button>
      </form>

    {:else if $screen === 'login'}
      <h2>Unlock {$profile}</h2>
      <form class="stack" on:submit|preventDefault={() => run(() => login(password))}>
        <label>Password<input type="password" bind:value={password} autofocus /></label>
        <button class="primary" type="submit" disabled={busy || !password}>{busy ? 'Unlocking…' : 'Unlock'}</button>
      </form>
      <div class="row">
        <button class="ghost" on:click={() => go('profiles')}>Other profile</button>
        <button class="ghost" on:click={() => go('restore')}>Forgot password</button>
      </div>

    {:else if $screen === 'create' || $screen === 'restore'}
      <div class="seg" role="tablist" aria-label="New or existing identity">
        <button role="tab" class:active={$screen === 'create'} aria-selected={$screen === 'create'} on:click={() => go('create')}>New identity</button>
        <button role="tab" class:active={$screen === 'restore'} aria-selected={$screen === 'restore'} on:click={() => go('restore')}>I have a recovery phrase</button>
      </div>
    {/if}

    {#if $screen === 'create'}
      <h2>Create an identity</h2>
      <p class="muted small">
        The password encrypts your keys on this computer. It cannot be reset; the recovery phrase shown next is the only backup.
      </p>
      <form class="stack" on:submit|preventDefault={submitCreate}>
        <label>Password<input type="password" bind:value={password} autofocus /></label>
        <label>Repeat password<input type="password" bind:value={confirm} /></label>
        <button class="primary" type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create'}</button>
      </form>
      {#if $profiles.length}<div class="row"><button class="ghost" on:click={() => go('profiles')}>Back</button></div>{/if}

    {:else if $screen === 'restore'}
      <h2>Restore from recovery phrase</h2>
      <p class="muted small">Type or paste the 12 or 24 words in their order. The same phrase restores the same identity and wallet as in the app or on a server node.</p>
      <form class="stack" on:submit|preventDefault={submitRestore}>
        <label>Recovery phrase<textarea rows="3" bind:value={phrase} spellcheck="false" autocomplete="off" placeholder="word word word …" /></label>
        {#if phraseCheck}<p class="small" class:error={phrase.trim().split(/\s+/).length >= 12} class:faint={phrase.trim().split(/\s+/).length < 12}>{phraseCheck}</p>
        {:else if phraseOk}<p class="small ok">The phrase is valid.</p>{/if}
        <details>
          <summary class="small muted">The phrase was made with an extra passphrase</summary>
          <label class="pp">Extra passphrase<input type="password" bind:value={passphrase} autocomplete="off" /></label>
        </details>
        <label>New password<input type="password" bind:value={password} /></label>
        <label>Repeat password<input type="password" bind:value={confirm} /></label>
        <button class="primary" type="submit" disabled={busy}>{busy ? 'Restoring…' : 'Restore'}</button>
      </form>
      {#if $profiles.length}<button class="ghost" on:click={() => go('profiles')}>Back</button>{/if}

    {:else if $screen === 'phrase'}
      <h2>Your recovery phrase</h2>
      <p class="muted small">
        Write these words down in order and keep them offline. Anyone with them controls your identity and wallet.
        They are shown only once.
      </p>
      <ol class="words">
        {#each $recoveryPhrase.split(' ') as w}<li class="mono">{w}</li>{/each}
      </ol>
      <label class="row"><input type="checkbox" bind:checked={saved} /> I have written the phrase down</label>
      <button class="primary" disabled={!saved || busy} on:click={() => run(phraseSaved)}>Continue</button>
    {/if}

    {#if error}<p class="error small">{error}</p>{/if}
  </div>
</div>

<style>
  .wrap {
    height: 100%; display: grid; place-items: center; overflow-y: auto; padding: 24px;
    background:
      radial-gradient(600px 400px at 20% 10%, rgba(29, 155, 240, 0.12), transparent 70%),
      radial-gradient(500px 380px at 85% 90%, rgba(29, 155, 240, 0.10), transparent 70%),
      var(--bg);
  }
  .box {
    width: 400px; padding: 28px; border-radius: 20px;
    background: color-mix(in srgb, var(--panel) 85%, transparent);
    border: 1px solid var(--line-soft);
    box-shadow: 0 24px 60px rgba(0, 0, 0, 0.45);
    backdrop-filter: blur(12px);
    animation: fade-up 0.25s ease-out;
  }
  .hero { display: flex; gap: 14px; align-items: center; margin-bottom: 4px; }
  .seg { display: flex; padding: 3px; gap: 3px; border-radius: 12px; background: var(--raised); border: 1px solid var(--line-soft); }
  .seg button { flex: 1; border: none; background: transparent; color: var(--muted); padding: 7px 10px; border-radius: 9px; }
  .seg button:hover:not(:disabled) { background: var(--hover); border: none; }
  .seg button.active, .seg button.active:hover:not(:disabled) { background: var(--accent-soft); color: var(--text); }
  details summary { cursor: pointer; }
  .pp { margin-top: 8px; }
  .mark {
    width: 44px; height: 44px; border-radius: 13px; flex: none;
    background: radial-gradient(circle at 30% 30%, rgba(255, 255, 255, 0.55), transparent 45%), linear-gradient(135deg, var(--accent), var(--accent-2));
    box-shadow: 0 8px 24px rgba(29, 155, 240, 0.4);
  }
  .left { text-align: left; }
  .words {
    display: grid; grid-template-columns: repeat(3, 1fr); gap: 6px 16px;
    margin: 0; padding: 12px 12px 12px 32px; background: var(--panel);
    border: 1px solid var(--line); border-radius: var(--radius);
  }
  .words li { color: var(--text); font-size: 13px; }
  .words li::marker { color: var(--faint); font-size: 11px; }
</style>
