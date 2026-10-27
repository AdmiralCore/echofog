import { writable, get } from 'svelte/store';
import { App, on, errText } from '../api.js';
import { play } from '../sounds.js';
import { callState } from '../services/call.service.js';
import { ringIncoming, incomingCall, declineIncomingCall, convergeGroupCall, peerInfo } from './call.store.js';
import { tr } from '../i18n/index.js';
import { clearTransfers, onGroupPost } from './files.js';
import { saved } from './saved.js';

const inCall = () => get(callState).inCall;

const CALL_INVITE = 'fogcall://';

// Chats run on the core's conversation log (ChatList, ChatHistory, ChatSend… and the conv:change
// event): every chat, direct or group, is a list of messages that can be answered, commented,
// edited, deleted, reacted to and pinned. See echofog-core-oss/docs/design/conversation-log.md.

/** Conversations: { id ('d:<peer>' | 'g:<group>'), kind: 'direct' | 'group', ref, name, peerId, pubKey, online,
 *  last (preview line), ts, unread, readOnly, request (a stranger who wrote first), blocked,
 *  upgraded (direct: the other side runs the log; group: every member does; null: nothing written yet) } */
export const chats = writable([]);
export const activeId = writable('');
/** Group calls going on right now, by chat id ('g:<group>'): { callId, coordinator, from } */
export const activeGroupCalls = writable({});

function setGroupCall(chatId, call) {
  activeGroupCalls.update((m) => {
    if (call) return { ...m, [chatId]: call };
    const { [chatId]: _, ...rest } = m;
    return rest;
  });
}

// the core forgets a call nobody announced for a while: ask it, so finished calls disappear
async function refreshGroupCalls() {
  for (const c of get(chats)) {
    if (c.kind !== 'group') continue;
    try {
      const call = await App.ActiveGroupCall(c.groupId);
      setGroupCall(c.id, call && call.callId ? { callId: call.callId, coordinator: call.coordinator, from: call.from } : null);
    } catch (_) { /* node not started */ }
  }
}
/** Messages of the open conversation, oldest first (see norm). Comments in threads are not here. */
export const messages = writable([]);
/** Whether the open conversation has older messages than those loaded. */
export const hasOlder = writable(false);
/** Pinned messages of the open conversation, most recently pinned first. */
export const pins = writable([]);
/** The open thread: { root (id), messages (oldest first) }, or null. */
export const thread = writable(null);
/** Messages answered by loaded ones that are not loaded themselves, by id (null: not there). */
export const quotes = writable({});
export const chatError = writable('');
/** A hint that is not an error, e.g. that members are offline and get the message later. */
export const chatNote = writable('');
/** Goes up whenever the member list or the roles of some group changed. */
export const membersVersion = writable(0);

let listening = false;
let olderCursor = '';

const MSG_FILE = 4;
const MSG_SYSTEM = 100;
const PAGE = 50;

/** The core's name for a chat. */
export const refOf = (conv) => (conv.kind === 'group' ? { kind: 'group', id: conv.groupId } : { kind: 'dm', id: conv.peerId });
const chatIdOf = (ref) => (ref.kind === 'group' ? 'g:' : 'd:') + ref.id;

// what a file message offers: the transfer it names, the file's name and size
function fileOf(m) {
  let meta = null;
  if (m.metadata) {
    try { meta = JSON.parse(m.metadata); } catch (_) { /* not JSON: name only */ }
  }
  const g = meta && meta.group_file;
  if (g) return { fid: g.id, name: g.name, size: Number(g.size) || 0, mime: g.mime || '' };
  if (m.type !== MSG_FILE) return null;
  meta = meta || {};
  return { fid: meta.custom?.fid || '', name: meta.file_name || m.text || '', size: Number(meta.file_size) || 0, mime: meta.mime_type || '' };
}

/** A message as the screen shows it, from the core's ChatMessage. */
export function norm(m) {
  const call = (m.text || '').startsWith(CALL_INVITE);
  const state = m.mine ? m.state || '' : '';
  return {
    id: m.id,
    lamport: m.lamport || 0,
    mine: !!m.mine,
    sender: m.mine ? '' : m.author,
    text: call ? (m.mine ? tr('chats.outgoingCall') : tr('chats.incomingCall')) : m.text || '',
    system: call || m.type === MSG_SYSTEM,
    type: m.type || 0,
    ts: m.ts || 0,
    file: m.deleted ? null : fileOf(m),
    state,
    pending: !!state && state !== 'delivered' && state !== 'expired',
    failed: state === 'expired',
    replyTo: m.replyTo || '',
    thread: m.thread || '',
    edited: !!m.edited,
    editedAt: m.editedAt || 0,
    deleted: !!m.deleted,
    withheld: !!m.withheld,
    pinned: !!m.pinned,
    reactions: m.reactions || [],
    replies: m.replies || 0,
  };
}

/** The line a chat list shows for a message. */
export const preview = (m) => (!m ? '' : m.deleted ? tr('chats.deletedMsg') : m.file ? tr('files.lastFile', { name: m.file.name }) : m.text);

// The core's order, oldest first: by Lamport clock; history from before the log (clock 0) by time;
// then by id, the larger first. Comments in a thread: by clock, then by id.
function before(a, b) {
  if (a.lamport !== b.lamport) return a.lamport - b.lamport;
  if (a.lamport === 0 && a.ts !== b.ts) return a.ts - b.ts;
  return a.id > b.id ? -1 : a.id < b.id ? 1 : 0;
}
function beforeInThread(a, b) {
  return a.lamport - b.lamport || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
}

// puts m in place in list (by id), or in order; one older than all of list belongs to a page not
// loaded yet when there is one, and is left out
function upsert(list, m, cmp, olderUnloaded) {
  const i = list.findIndex((x) => x.id === m.id);
  if (i >= 0) {
    const next = list.slice();
    next[i] = m;
    return next;
  }
  if (olderUnloaded && list.length && cmp(m, list[0]) < 0) return list;
  let j = list.length;
  while (j > 0 && cmp(m, list[j - 1]) < 0) j--;
  return [...list.slice(0, j), m, ...list.slice(j)];
}

function patch(id, fn) {
  chats.update((list) => list.map((c) => (c.id === id ? fn(c) : c)));
}

// the parts of a list row the log knows
const fromLog = (d) => ({
  last: d.last ? preview(norm(d.last)) : '',
  lastId: d.last ? d.last.id : '',
  ts: d.lastTs || 0,
  unread: d.unread || 0,
  upgraded: !!d.upgraded,
});

export async function loadChats() {
  chatError.set('');
  const items = [];
  let blocked = new Set();
  const list = (v) => (Array.isArray(v) ? v : []);
  try { blocked = new Set(list(await App.GetBlockedPeers()).map((b) => b.peer_id)); } catch (_) { /* older core */ }
  let requests = [];
  try { requests = list(await App.GetMessageRequests()); } catch (_) { /* older core */ }
  const logged = {};
  try {
    for (const d of list(await App.ChatList())) logged[chatIdOf(d.ref)] = d;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadMessages')));
  }
  const people = [];
  try {
    for (const c of (await App.GetContacts()) || []) people.push({ ...c, request: false });
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadContacts')));
  }
  for (const r of requests) {
    if (!people.some((c) => c.peer_id === r.peer_id)) people.push({ peer_id: r.peer_id, pub_key: r.pub_key, alias: '', request: true });
  }
  for (const c of people) {
    const id = 'd:' + c.peer_id;
    const d = logged[id];
    items.push({
      id, kind: 'direct', ref: { kind: 'dm', id: c.peer_id }, name: c.alias || c.peer_id.slice(0, 8),
      peerId: c.peer_id, pubKey: c.pub_key, online: !!c.is_online, last: '', lastId: '', ts: 0, unread: 0, upgraded: null,
      ...(d ? fromLog(d) : {}),
      request: c.request, blocked: blocked.has(c.peer_id),
    });
  }
  try {
    for (const g of (await App.ListGroups()) || []) {
      const id = 'g:' + g.id;
      const d = logged[id];
      items.push({
        id, kind: 'group', ref: { kind: 'group', id: g.id }, name: g.title, groupId: g.id, members: g.members, myRole: g.myRole,
        readOnly: g.readOnly, groupKind: g.kind, last: '', lastId: '', ts: g.createdAt || 0, unread: 0, upgraded: null,
        ...(d ? fromLog(d) : {}),
      });
    }
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadGroups')));
  }
  chats.set(items);
  listen();
}

// a change of one chat: its row in the list and, when it is open, its messages
async function changed(ev) {
  const id = chatIdOf(ev.ref);
  const d = ev.conversation;
  let row = get(chats).find((c) => c.id === id);
  const fresh = !row;
  if (fresh) {
    await loadChats(); // a stranger wrote first (a message request), or a group we just joined
    row = get(chats).find((c) => c.id === id);
    if (!row) return;
  }
  const msgs = (ev.messages || []).map(norm);
  const open = get(activeId) === id;
  const arrived = d.last && !d.last.mine && (fresh || d.last.id !== row.lastId);
  if (arrived && msgs.some((m) => m.id === d.last.id && !m.system)) play('receive', { quiet: inCall() });
  patch(id, (c) => ({ ...c, ...fromLog(d), unread: open && get(focused) ? 0 : d.unread || 0 }));
  if (!open) return;

  messages.update((list) => msgs.filter((m) => !m.thread).reduce((l, m) => upsert(l, m, before, get(hasOlder)), list));
  const th = get(thread);
  if (th) {
    const inThread = msgs.filter((m) => m.thread === th.root);
    if (inThread.length) thread.set({ ...th, messages: inThread.reduce((l, m) => upsert(l, m, beforeInThread, false), th.messages) });
  }
  const q = get(quotes);
  if (msgs.some((m) => m.id in q)) quotes.set({ ...q, ...Object.fromEntries(msgs.filter((m) => m.id in q).map((m) => [m.id, m])) });
  if (msgs.some((m) => m.pinned || get(pins).some((p) => p.id === m.id))) loadPins();
}

/** Whether the window is in front: messages read in a background window do not count as read. */
const focused = writable(typeof document === 'undefined' || document.hasFocus());
if (typeof window !== 'undefined') {
  window.addEventListener('focus', () => focused.set(true));
  window.addEventListener('blur', () => focused.set(false));
}

function listen() {
  if (listening) return;
  listening = true;

  on('conv:change', (ev) => { if (ev && ev.ref) changed(ev); });

  // a message of ours moved on: handed to a relay, taken by the recipient's device, confirmed, or given up
  on('message:state', (ev) => {
    if (!ev || !ev.id) return;
    const set = (m) => (m.id === ev.id ? { ...m, state: ev.state, pending: ev.state !== 'delivered' && ev.state !== 'expired', failed: ev.state === 'expired' } : m);
    messages.update((list) => list.map(set));
    thread.update((th) => (th ? { ...th, messages: th.messages.map(set) } : th));
  });

  // an invitation that took too long to arrive (from a relay's mailbox): it does not ring
  on('call:missed', (ev) => {
    if (!ev) return;
    chatNote.set(tr('chats.note.missedFrom', { name: nameOfPeer(ev.from) }));
  });

  on('group:event', async (ev) => {
    if (!ev) return;
    const id = 'g:' + ev.groupId;
    switch (ev.kind) {
      case 'members':
      case 'added':
        await loadChats();
        membersVersion.update((n) => n + 1);
        break;
      case 'removed':
        if (get(activeId) === id) closeOpen();
        await loadChats();
        if (ev.reason === 'deleted') chatNote.set(ev.title ? tr('chats.note.deleted', { title: ev.title }) : tr('chats.note.groupDeleted'));
        break;
      case 'history':
        // earlier messages came in at once (joining a group): read the newest page again
        if (get(activeId) === id) await reload();
        break;
      case 'callend':
        if (get(activeGroupCalls)[id]?.callId === ev.callId) setGroupCall(id, null);
        // the call is over: if it was still ringing here, stop
        if (get(incomingCall)?.callId === ev.callId) {
          declineIncomingCall();
          chatNote.set(ev.title ? tr('chats.note.missedCall', { title: ev.title }) : tr('chats.note.missedCallGroup'));
        }
        break;
      case 'call':
        // somebody started a call in a group we are in
        setGroupCall(id, { callId: ev.callId, coordinator: ev.coordinator, from: ev.from });
        // already in a call of this group: merge the two instead of ringing
        if ((get(callState).inCall || get(callState).connecting) && get(peerInfo).chatId === id) {
          await convergeGroupCall(id, ev.callId, ev.coordinator);
          break;
        }
        ringIncoming({
          callId: ev.callId, coordinator: ev.coordinator, from: ev.from, chatId: id,
          alias: `${ev.title || tr('chats.groupFallback')}: ${nameOfPeer(ev.from)}`,
        });
        break;
      case 'callactive':
        // a call already under way that we had not heard of: show it, do not ring
        setGroupCall(id, { callId: ev.callId, coordinator: ev.coordinator, from: ev.from });
        await convergeGroupCall(id, ev.callId, ev.coordinator);
        break;
      default:
    }
  });

  refreshGroupCalls();
  setInterval(refreshGroupCalls, 20_000);
}

function nameOfPeer(peerId) {
  const c = get(chats).find((x) => x.peerId === peerId);
  return c ? c.name : (peerId || '').slice(0, 8);
}

const openConv = () => get(chats).find((c) => c.id === get(activeId));

function closeOpen() {
  activeId.set('');
  messages.set([]);
  hasOlder.set(false);
  pins.set([]);
  thread.set(null);
  quotes.set({});
}

export async function openChat(id) {
  closeOpen();
  activeId.set(id);
  chatNote.set('');
  await reload();
  loadPins();
}

// the newest page of the open chat
async function reload() {
  const conv = openConv();
  if (!conv) return;
  try {
    const page = await App.ChatHistory(conv.ref, '', PAGE);
    if (get(activeId) !== conv.id) return;
    olderCursor = page.next || '';
    hasOlder.set(!!olderCursor);
    messages.set((page.messages || []).map(norm).reverse());
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadMessages')));
  }
}

let loadingOlder = false;
/** Loads the page before the oldest loaded message; false when there is none. */
export async function loadOlder() {
  const conv = openConv();
  if (!conv || !olderCursor || loadingOlder) return false;
  loadingOlder = true;
  try {
    const page = await App.ChatHistory(conv.ref, olderCursor, PAGE);
    if (get(activeId) !== conv.id) return false;
    olderCursor = page.next || '';
    hasOlder.set(!!olderCursor);
    const older = (page.messages || []).map(norm).reverse();
    messages.update((list) => [...older.filter((m) => !list.some((x) => x.id === m.id)), ...list]);
    return older.length > 0;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadMessages')));
    return false;
  } finally {
    loadingOlder = false;
  }
}

export async function loadPins() {
  const conv = openConv();
  if (!conv) return;
  try {
    const list = ((await App.ChatPins(conv.ref)) || []).map(norm);
    if (get(activeId) === conv.id) pins.set(list);
  } catch (_) { /* nothing pinned */ }
}

/** Fetches a message that a loaded one answers, when it is not loaded itself. */
export async function fetchQuote(id) {
  const conv = openConv();
  if (!conv || !id || id in get(quotes)) return;
  quotes.update((q) => ({ ...q, [id]: null }));
  try {
    const m = norm(await App.ChatMessage(conv.ref, id));
    if (get(activeId) === conv.id) quotes.update((q) => ({ ...q, [id]: m }));
  } catch (_) { /* not here (yet): shown as unavailable */ }
}

/** Opens the comments under a message. */
export async function openThread(rootId) {
  const conv = openConv();
  if (!conv) return;
  thread.set({ root: rootId, messages: [] });
  try {
    const page = await App.ChatThread(conv.ref, rootId, '', 200);
    const th = get(thread);
    if (th && th.root === rootId) thread.set({ ...th, messages: (page.messages || []).map(norm).reduce((l, m) => upsert(l, m, beforeInThread, false), th.messages) });
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadMessages')));
  }
}
export const closeThread = () => thread.set(null);

/** Sends a message to the open chat: plain, an answer (replyTo) or a comment (threadRoot). */
export async function send(text, { replyTo = '', threadRoot = '' } = {}) {
  text = text.trim();
  const conv = openConv();
  if (!text || !conv) return false;
  chatError.set('');
  chatNote.set('');
  try {
    // answering a request accepts it: the stranger becomes a contact
    if (conv.kind === 'direct' && conv.request) await acceptRequest(conv, '');
    const m = norm(await App.ChatSend(conv.ref, { text, replyTo, thread: threadRoot }));
    // shown at once; the core's change event follows with the same message
    if (get(activeId) === conv.id && m.id) {
      if (m.thread) thread.update((th) => (th && th.root === m.thread ? { ...th, messages: upsert(th.messages, m, beforeInThread, false) } : th));
      else messages.update((list) => upsert(list, m, before, false));
    }
    play('send', { quiet: inCall() });
    return true;
  } catch (err) {
    chatError.set(opError(err, 'chats.err.notSent'));
    return false;
  }
}

// the core's refusals in the user's words
function opError(err, fallback) {
  const s = errText(err, tr(fallback));
  if (/not every member/.test(s)) return tr('chats.err.needsUpgrade');
  if (/not allowed/.test(s)) return tr('chats.err.notAllowed');
  if (/bad event body/.test(s)) return tr('chats.err.badBody');
  return s;
}

async function op(fn, fallback) {
  const conv = openConv();
  if (!conv) return false;
  chatError.set('');
  try {
    await fn(conv.ref);
    return true;
  } catch (err) {
    chatError.set(opError(err, fallback));
    return false;
  }
}

export const editMessage = (id, text) => op((ref) => App.ChatEdit(ref, id, text.trim()), 'chats.err.notEdited');
export const deleteMessage = (id) => op((ref) => App.ChatDelete(ref, id), 'chats.err.notDeleted');
export const react = (id, emoji, on) => op((ref) => App.ChatReact(ref, id, emoji, on), 'chats.err.notReacted');
export const pinMessage = (id, on) => op((ref) => App.ChatPin(ref, id, on), 'chats.err.notPinned');

/** Records that the open chat was read up to message id. */
export async function markRead(id) {
  const conv = openConv();
  if (!conv || !id) return;
  patch(conv.id, (c) => ({ ...c, unread: 0 }));
  try { await App.ChatMarkRead(conv.ref, id); } catch (_) { /* counted again next time */ }
}

// a file posted in a group shows like a message we sent (the log brings the message itself)
onGroupPost(async (_conv, m) => {
  if (m.queued > 0) chatNote.set(tr('chats.note.queued', { n: m.queued }));
  if (m.failed > 0) chatError.set(tr('chats.err.failed', { n: m.failed }));
  play('send', { quiet: inCall() });
});

/** Adds a contact from a pasted/scanned link or key; name overrides the one in the link. */
export async function addContact(key, name) {
  const c = await App.AddContactByKey(key, name);
  await loadChats();
  await openChat('d:' + c.peer_id);
}

export async function deleteContact(conv) {
  await App.DeleteContact(conv.peerId);
  closeOpen();
  await loadChats();
}

/** Makes the stranger behind a message request a contact. */
export async function acceptRequest(conv, name) {
  await App.AcceptMessageRequest(conv.peerId, (name || '').trim());
  patch(conv.id, (c) => ({ ...c, request: false, name: (name || '').trim() || c.name }));
}

/** Removes a request without blocking; the conversation goes until the stranger writes again. */
export async function dismissRequest(conv) {
  await App.DismissMessageRequest(conv.peerId);
  closeOpen();
  await loadChats();
}

/** Blocks a person: nothing from them arrives any more, and they are not told. */
export async function blockPeer(conv) {
  await App.BlockPeer(conv.peerId);
  await loadChats();
}

export async function unblockPeer(conv) {
  await App.UnblockPeer(conv.peerId);
  await loadChats();
}

export async function createGroup(title, kind) {
  const g = await App.CreateGroup(title, '', kind);
  await loadChats();
  await openChat('g:' + g.id);
}

export async function joinGroup(invite) {
  const g = await App.JoinGroup(invite.trim());
  await loadChats();
  await openChat('g:' + g.id);
}

export async function leaveGroup(conv) {
  await App.LeaveGroup(conv.groupId);
  closeOpen();
  await loadChats();
}

export function closeChats() {
  closeOpen();
  chats.set([]);
  clearTransfers(); // another profile may log in next
  saved.set([]);
}
