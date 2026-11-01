import { writable, get } from 'svelte/store';
import { App, waitForBridge, errText } from '../api.js';
import { tr } from '../i18n/index.js';
import { log } from '../uilog.js';

// screen: 'loading' | 'profiles' | 'login' | 'create' | 'restore' | 'phrase' | 'access' | 'app'
export const screen = writable('loading');
export const profiles = writable([]);
export const profile = writable('');
export const recoveryPhrase = writable('');
export const me = writable({ id: '', pubKey: '' });
// why the profile remembered on this computer asks the password after all: { reason, error } or null
export const unlockNote = writable(null);

export async function boot() {
  await waitForBridge();
  if (await autoUnlock()) return;
  try {
    const list = (await App.GetProfiles()) || [];
    profiles.set(list);
    if (list.length === 0) {
      await choose('default');
      return;
    }
    screen.set('profiles');
  } catch (_) {
    screen.set('profiles');
  }
}

// autoUnlock opens the profile remembered on this computer without the password, once per start
// of the app. True when it took over: the app runs, or that profile's password screen says why not.
async function autoUnlock() {
  let r;
  try { r = await App.AutoUnlock(); } catch (_) { return false; }
  if (!r?.profile) return false;
  try { profiles.set((await App.GetProfiles()) || []); } catch (_) { /* the password screen still works */ }
  profile.set(r.profile);
  if (r.unlocked) {
    try {
      await start();
      return true;
    } catch (err) {
      r = { reason: 'failed', error: errText(err) };
    }
  }
  unlockNote.set({ reason: r.reason || 'failed', error: r.error || '' });
  screen.set('login');
  return true;
}

export async function choose(name) {
  name = (name || '').trim();
  if (!name) throw new Error(tr('auth.enterProfileName'));
  await App.SelectProfile(name);
  profile.set(name);
  const registered = await App.IsRegistered();
  screen.set(registered ? 'login' : 'create');
}

export async function login(password, remember = false) {
  await App.Login(password);
  unlockNote.set(null);
  // asked on the password screen: the payments keep asking the password
  if (remember) {
    try { await App.RememberUnlock(password, true); } catch (err) { log.warn('auth', 'could not remember the unlock', { err }); }
  }
  await start();
}

export async function create(password) {
  const phrase = await App.Register(password, '');
  recoveryPhrase.set(phrase);
  screen.set('phrase');
}

export async function phraseSaved() {
  recoveryPhrase.set('');
  screen.set('access'); // a new user chooses who can find this computer before it goes online
}

/** The welcome choice after registering: public or hidden, then the node starts with it. */
export async function chooseAccess(mode) {
  await App.SetAccessMode(mode);
  await start();
}

export async function restore(phrase, password, passphrase = '') {
  await App.Recover(phrase.trim().split(/\s+/).join(' '), passphrase, password);
  await start();
}

async function start() {
  await App.StartNode();
  me.set({ id: await App.GetMyID(), pubKey: await App.GetMyPubKey() });
  screen.set('app');
}

export async function logout() {
  try { await App.Logout(); } catch (_) { /* already stopped */ }
  me.set({ id: '', pubKey: '' });
  profile.set('');
  await boot();
}

/** Lock: the node stops and the same profile's password screen comes back; the remembered unlock stays. */
export async function lock() {
  try { await App.Lock(); } catch (_) { /* already stopped */ }
  me.set({ id: '', pubKey: '' });
  screen.set('login');
}

export async function removeProfile(name, appPassword) {
  await App.DeleteProfile(name, appPassword);
  profiles.set((await App.GetProfiles()) || []);
  if (get(profile) === name) profile.set('');
}

export { errText };
