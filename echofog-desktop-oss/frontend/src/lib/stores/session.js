import { writable, get } from 'svelte/store';
import { App, waitForBridge, errText } from '../api.js';

// screen: 'loading' | 'profiles' | 'login' | 'create' | 'restore' | 'phrase' | 'app'
export const screen = writable('loading');
export const profiles = writable([]);
export const profile = writable('');
export const recoveryPhrase = writable('');
export const me = writable({ id: '', pubKey: '' });

export async function boot() {
  await waitForBridge();
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

export async function choose(name) {
  name = (name || '').trim();
  if (!name) throw new Error('Enter a profile name');
  await App.SelectProfile(name);
  profile.set(name);
  const registered = await App.IsRegistered();
  screen.set(registered ? 'login' : 'create');
}

export async function login(password) {
  await App.Login(password);
  await start();
}

export async function create(password) {
  const phrase = await App.Register(password, '');
  recoveryPhrase.set(phrase);
  screen.set('phrase');
}

export async function phraseSaved() {
  recoveryPhrase.set('');
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

export async function removeProfile(name) {
  await App.DeleteProfile(name);
  profiles.set((await App.GetProfiles()) || []);
  if (get(profile) === name) profile.set('');
}

export { errText };
