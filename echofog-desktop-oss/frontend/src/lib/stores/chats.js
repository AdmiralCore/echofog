import { writable, get } from 'svelte/store';
import { App, on, errText } from '../api.js';
import { play } from '../sounds.js';
import { callState } from '../services/call.service.js';
import { ringIncoming, incomingCall, declineIncomingCall, convergeGroupCall, peerInfo } from './call.store.js';
import { tr } from '../i18n/index.js';
import { clearTransfers, onGroupPost } from './files.js';
import { saved } from './saved.js';

const inCall = () => get(callState).inCall;

// Chats run on the core's conversation log (ChatList, ChatHistory, ChatSend… and the conv:change
// event): every chat, direct or group, is a list of messages that can be answered, commented,
// edited, deleted, reacted to and pinned. See echofog-core-oss/docs/design/conversation-log.md.

/** Conversations: { id ('d:<peer>' | 'g:<group>'), kind: 'direct' | 'group', groupKind ('group' | 'channel'), ref,
 *  name, peerId, pubKey, groupId, online, last (preview line), lastId, ts, unread, request (a stranger who wrote
 *  first), blocked, myRole, members, upgraded (the chat runs on the log), can (what I may do here, from the core:
 *  send, files, react, pin, manage, invite, call), readOnly (= !can.send) }. The core decides every right. */
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
/** Chats whose new messages make no sound (kept on this device). */
export const muted = writable(new Set((() => {
  try { return JSON.parse(localStorage.getItem('echofog.muted') || '[]'); } catch (_) { return []; }
})()));
export function toggleMuted(id) {
  muted.update((s) => {
    const next = new Set(s);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    try { localStorage.setItem('echofog.muted', JSON.stringify([...next])); } catch (_) { /* private mode */ }
    return next;
  });
}

/** The open group's channels and voice rooms (ChatChannels), and the channel shown ('' the main one). */
export const channels = writable([]);
export const activeChannel = writable('');
/** The open group's main channel settings (ChatMainChannel): { name, topic, readOnly, slowSecs, open, canSend }. */
export const mainChannel = writable({});
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

const PAGE = 50;

/** The core's name for a chat. */
export const refOf = (conv) => (conv.kind === 'group' ? { kind: 'group', id: conv.groupId } : { kind: 'dm', id: conv.peerId });
const chatIdOf = (ref) => (ref.kind === 'group' ? 'g:' : 'd:') + ref.id;

// the furthest state each message of mine was reported in, for the messages the report beat to
// the screen (a fast confirmation comes before ChatSend returns)
const reported = new Map();
function report(id, state) {
  reported.set(id, furthest(reported.get(id) || '', state));
  if (reported.size > 500) reported.delete(reported.keys().next().value);
}

/** A message as the screen shows it, from the core's ChatMessage. */
export function norm(m) {
  const call = m.kind === 'call';
  const state = m.mine ? furthest(m.state || '', reported.get(m.id) || '') : '';
  return {
    id: m.id,
    lamport: m.lamport || 0,
    mine: !!m.mine,
    sender: m.mine ? '' : m.author,
    text: call ? (m.mine ? tr('chats.outgoingCall') : tr('chats.incomingCall')) : m.text || '',
    system: call || m.kind === 'system',
    type: m.type || 0,
    ts: m.ts || 0,
    file: m.deleted || !m.file ? null : {
      fid: m.file.id, name: m.file.name, size: m.file.size || 0, mime: m.file.mime || '',
      // a picture's thumbnail, sent with the offer: the chat shows it before the file is fetched
      thumb: m.file.thumb ? 'data:image/jpeg;base64,' + m.file.thumb : '', w: m.file.w || 0, h: m.file.h || 0,
    },
    can: m.can || {},
    state,
    pending: !!state && state !== 'delivered' && state !== 'expired',
    failed: state === 'expired',
    replyTo: m.replyTo || '',
    thread: m.thread || '',
    channel: m.channel || '',
    edited: !!m.edited,
    editedAt: m.editedAt || 0,
    deleted: !!m.deleted,
    withheld: !!m.withheld,
    pinned: !!m.pinned,
    reactions: m.reactions || [],
    replies: m.replies || 0,
    threadTitle: m.threadTitle || '',
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

// How far a message of mine got. A state never goes back: the events can cross (a retry that went
// out as the confirmation came in, or ChatSend's answer arriving after the confirmation).
const REACHED = { keys: 1, offline: 1, relayed: 2, sent: 3, delivered: 4, expired: 5 };
const furthest = (a, b) => ((REACHED[b] || 0) >= (REACHED[a] || 0) ? b : a);
const withState = (m, state) => ({ ...m, state, pending: !!state && state !== 'delivered' && state !== 'expired', failed: state === 'expired' });

// puts m in place in list (by id), or in order; one older than all of list belongs to a page not
// loaded yet when there is one, and is left out
function upsert(list, m, cmp, olderUnloaded) {
  const i = list.findIndex((x) => x.id === m.id);
  if (i >= 0) {
    const next = list.slice();
    next[i] = list[i].state && m.state ? withState(m, furthest(list[i].state, m.state)) : m;
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

/** A picture kept as base64 (a contact's), as an image address; '' without one. */
export function avatarSrc(b64) {
  if (!b64) return '';
  const type = b64.startsWith('/9j/') ? 'jpeg' : b64.startsWith('iVBOR') ? 'png' : b64.startsWith('R0lG') ? 'gif' : 'webp';
  return `data:image/${type};base64,${b64}`;
}

// a chat-list row from the core's entry
function rowOf(d) {
  const can = d.can || {};
  const group = d.ref.kind === 'group';
  return {
    id: chatIdOf(d.ref), kind: group ? 'group' : 'direct', groupKind: d.kind === 'channel' ? 'channel' : 'group', ref: d.ref,
    name: d.name || (group ? d.title : '') || d.ref.id.slice(0, 8), peerId: group ? '' : d.ref.id, pubKey: d.pubKey || '', groupId: group ? d.ref.id : '',
    online: !!d.online, blocked: !!d.blocked, request: !!d.request, myRole: d.role || '', members: d.members || 0,
    avatar: avatarSrc(d.avatar),
    can, readOnly: !can.send,
    last: d.last ? preview(norm(d.last)) : '', lastId: d.last ? d.last.id : '', ts: d.lastTs || 0, unread: d.unread || 0,
    upgraded: !!d.upgraded,
  };
}

// the newest change seen per chat: one that lost a race to a newer one is stale
const revs = new Map();

export async function loadChats() {
  chatError.set('');
  try {
    const rows = await App.ChatList();
    chats.set((Array.isArray(rows) ? rows : []).map(rowOf));
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadMessages')));
  }
  listen();
}

// a change of one chat: its row in the list and, when it is open, its messages
async function changed(ev) {
  const id = chatIdOf(ev.ref);
  const d = ev.conversation;
  if (ev.rev) {
    if (ev.rev < (revs.get(id) || 0) && !(ev.messages || []).length) return;
    revs.set(id, Math.max(ev.rev, revs.get(id) || 0));
  }
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
  if (arrived && !get(muted).has(id) && msgs.some((m) => m.id === d.last.id && !m.system)) play('receive', { quiet: inCall() });
  patch(id, (c) => ({ ...c, ...rowOf(d), unread: open && get(focused) ? 0 : d.unread || 0 }));
  if (!open) return;
  if (ev.reload) {
    // the chat was computed again (who may do what changed): read it afresh
    await reloadOpen();
    return;
  }

  const here = get(activeChannel);
  messages.update((list) => msgs.filter((m) => !m.thread && m.channel === here).reduce((l, m) => upsert(l, m, before, get(hasOlder)), list));
  // a change with no messages is one of the group's own (a channel made, renamed or set up)
  if (row.kind === 'group' && (!msgs.length || msgs.some((m) => m.channel) || here)) loadChannels();
  const th = get(thread);
  if (th) {
    const inThread = msgs.filter((m) => m.thread === th.root);
    if (inThread.length) thread.set({ ...th, messages: inThread.reduce((l, m) => upsert(l, m, beforeInThread, false), th.messages) });
  }
  // a comment, a rename or a deleted root changes the threads list
  if (get(threadsShown) && msgs.some((m) => m.thread || m.replies || m.threadTitle || get(threads).some((x) => x.id === m.id))) threadsSoon();
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

  // a connection to a contact came up or went away
  on('peer:state', (ev) => {
    if (ev && ev.peer_id) patch('d:' + ev.peer_id, (c) => ({ ...c, online: !!ev.online }));
  });

  // a message of ours moved on: handed to a relay, taken by the recipient's device, confirmed, or given up
  on('message:state', (ev) => {
    if (!ev || !ev.id) return;
    report(ev.id, ev.state);
    const set = (m) => (m.id === ev.id ? withState(m, furthest(m.state, ev.state)) : m);
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
      case 'requests': // a request to join waits, or was decided
        membersVersion.update((n) => n + 1);
        break;
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
        if (ev.room) { if (get(activeId) === id) loadChannels(); break; } // a voice room's call
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
        if (ev.room) { if (get(activeId) === id) loadChannels(); break; } // a voice room: shown in the room's row
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
/** The core's name for the open chat's shown channel. */
const openRef = (conv) => (get(activeChannel) ? { ...conv.ref, channel: get(activeChannel) } : conv.ref);

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
  activeChannel.set('');
  channels.set([]);
  mainChannel.set({});
  activeId.set(id);
  chatNote.set('');
  await reload();
  loadPins();
  await loadChannels();
  // a group set to open on one of its channels
  const open = get(mainChannel).open;
  if (open && get(activeId) === id && !get(activeChannel)) openChannel(open);
}

/** Shows another channel of the open group ('' the main one). */
export async function openChannel(id) {
  if (get(activeChannel) === id) return;
  activeChannel.set(id);
  messages.set([]);
  hasOlder.set(false);
  thread.set(null);
  await reload();
}

/** Reads the open group's channels and voice rooms again (unread counts, calls going on). */
export async function loadChannels() {
  const conv = openConv();
  if (!conv || conv.kind !== 'group') return;
  try {
    const list = (await App.ChatChannels(conv.ref)) || [];
    if (get(activeId) !== conv.id) return;
    channels.set(list);
    if (get(activeChannel) && !list.some((c) => c.id === get(activeChannel) && !c.voice && !c.category)) openChannel(''); // removed
  } catch (_) { /* an older core: no channels */ }
  try {
    const main = (await App.ChatMainChannel(conv.ref)) || {};
    if (get(activeId) === conv.id) mainChannel.set(main);
  } catch (_) { /* an older core: no settings of the main channel */ }
}

/** Changes the open group's main channel: { name, topic, readOnly, slowSecs, open }. */
export async function saveMainChannel(spec) {
  const conv = openConv();
  if (!conv) return false;
  try {
    await App.ChatMainChannelSave(conv.ref, { id: '', name: (spec.name || '').trim(), voice: false, topic: (spec.topic || '').trim(), readOnly: !!spec.readOnly,
      position: 0, parent: '', category: false, slowSecs: spec.slowSecs || 0, open: spec.open || '', overrides: spec.overrides || [] });
    await loadChannels();
    return true;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
    return false;
  }
}

/** Puts a channel, a room or the main channel ('') of the open group in a category ('' none). */
export async function placeChannel(id, parent) {
  const conv = openConv();
  if (!conv) return false;
  try {
    await App.ChatChannelPlace(conv.ref, id, parent || '');
    await loadChannels();
    return true;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
    return false;
  }
}

/** Turns the open group's default headings (text, voice) into categories: [textId, voiceId], or null. */
export async function setupCategories() {
  const conv = openConv();
  if (!conv) return null;
  try {
    const ids = await App.ChatCategoriesSetup(conv.ref, tr('chats.text'), tr('chats.voice'));
    await loadChannels();
    return ids;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
    return null;
  }
}

/** Creates a category of the open group: a heading its channels and rooms sit under. */
export async function createCategory(name) {
  const conv = openConv();
  if (!conv || !name.trim()) return null;
  try {
    const ch = await App.ChatChannelSave(conv.ref, { id: '', name: name.trim(), voice: false, topic: '', readOnly: false, position: 0, parent: '', category: true, slowSecs: 0, open: '' });
    await loadChannels();
    return ch;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
    return null;
  }
}

/** Creates a channel (or a voice room) in the open group, in a category (parent) or none. */
export async function createChannel(name, voice, parent = '') {
  const conv = openConv();
  if (!conv || !name.trim()) return null;
  try {
    const ch = await App.ChatChannelSave(conv.ref, { id: '', name: name.trim(), voice: !!voice, topic: '', readOnly: false, position: 0, parent, category: false, slowSecs: 0, open: '' });
    await loadChannels();
    return ch;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
    return null;
  }
}

/** Changes a channel or a category of the open group: { id, name, topic, readOnly, position, parent, slowSecs, overrides }. */
export async function saveChannel(ch) {
  const conv = openConv();
  if (!conv) return false;
  try {
    await App.ChatChannelSave(conv.ref, { id: ch.id, name: ch.name.trim(), voice: !!ch.voice, topic: (ch.topic || '').trim(), readOnly: !!ch.readOnly,
      position: ch.position || 0, parent: ch.parent || '', category: !!ch.category, slowSecs: ch.slowSecs || 0, open: '', overrides: ch.overrides || [] });
    await loadChannels();
    return true;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
    return false;
  }
}

/** Puts a channel at index among the channels of its kind. */
export async function moveChannel(id, index) {
  const conv = openConv();
  if (!conv) return;
  try {
    await App.ChatChannelMove(conv.ref, id, index);
    await loadChannels();
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
  }
}

/** The open channel's threads (messages with comments), the most recently active first. */
export const threads = writable([]);
/** Whether the threads list is on screen: a change that touches a thread reads it again. */
export const threadsShown = writable(false);
let threadsTimer = null;
function threadsSoon() {
  if (threadsTimer) return;
  threadsTimer = setTimeout(() => { threadsTimer = null; if (get(threadsShown)) loadThreads(); }, 250);
}
export async function loadThreads() {
  const conv = openConv();
  if (!conv) return;
  try {
    const list = ((await App.ChatThreads(openRef(conv))) || []).map(norm);
    if (get(activeId) === conv.id) threads.set(list);
  } catch (_) { threads.set([]); }
}

/** Names the thread under a message ('' clears the name). */
export async function setThreadTitle(rootId, title) {
  const conv = openConv();
  if (!conv) return false;
  try {
    await App.ChatThreadTitle(conv.ref, rootId, title);
    return true;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.threadTitle')));
    return false;
  }
}

/** Removes a channel of the open group. */
export async function removeChannel(id) {
  const conv = openConv();
  if (!conv) return;
  try {
    await App.ChatChannelRemove(conv.ref, id);
    if (get(activeChannel) === id) await openChannel('');
    await loadChannels();
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.channel')));
  }
}

// the open chat read afresh: its newest page, its pins and its open thread
async function reloadOpen() {
  await reload();
  loadPins();
  const th = get(thread);
  if (th) openThread(th.root);
}

// the newest page of the open chat
async function reload() {
  const conv = openConv();
  if (!conv) return;
  try {
    const page = await App.ChatHistory(openRef(conv), '', PAGE);
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
    const page = await App.ChatHistory(openRef(conv), olderCursor, PAGE);
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
    const m = norm(await App.ChatSend(openRef(conv), { text, replyTo, thread: threadRoot }));
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
  try { await App.ChatMarkRead(openRef(conv), id); } catch (_) { /* counted again next time */ }
  loadChannels();
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
