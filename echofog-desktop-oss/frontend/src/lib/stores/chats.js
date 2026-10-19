import { toMs } from '../format.js';
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

/** Conversations: { id, kind: 'direct' | 'group', name, peerId, pubKey, online, last, ts, unread, readOnly } */
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
/** Messages of the open conversation: { id, mine, sender, text, ts, failed, pending } */
export const messages = writable([]);
export const chatError = writable('');
/** A hint that is not an error, e.g. that members are offline and get the message later. */
export const chatNote = writable('');
/** Goes up whenever the member list or the roles of some group changed. */
export const membersVersion = writable(0);

let listening = false;

const MSG_FILE = 4;

// what a file message offers: the transfer it names, the file's name and size
function fileOf(m) {
  if (m.type !== MSG_FILE) return null;
  let meta = {};
  try { meta = JSON.parse(m.metadata || '{}') || {}; } catch (_) { /* not JSON: name only */ }
  return { fid: meta.custom?.fid || '', name: meta.file_name || m.text || '', size: Number(meta.file_size) || 0, mime: meta.mime_type || '' };
}

/** The line a chat list shows for a message. */
export const preview = (m) => (m.file ? tr('files.lastFile', { name: m.file.name }) : m.text);

function directMessage(m) {
  const call = (m.text || '').startsWith(CALL_INVITE);
  return {
    id: m.id || 'm' + m.timestamp,
    mine: !!m.is_mine,
    sender: m.is_mine ? '' : m.sender_id,
    text: call ? (m.is_mine ? tr('chats.outgoingCall') : tr('chats.incomingCall')) : m.text,
    system: call,
    pending: !!m.pending,
    ts: toMs(m.timestamp),
    file: fileOf(m),
  };
}

function groupMessage(m) {
  return {
    id: m.id,
    mine: !!m.isMine,
    sender: m.isMine ? '' : m.sender,
    text: m.text,
    ts: m.timestamp,
    failed: m.failed > 0 && m.delivered === 0 && m.queued === 0,
    pending: m.queued > 0,
    file: m.file ? { fid: m.file.id, name: m.file.name, size: m.file.size, mime: m.file.mime || '' } : null,
  };
}

function patch(id, fn) {
  chats.update((list) => list.map((c) => (c.id === id ? fn(c) : c)));
}

export async function loadChats() {
  chatError.set('');
  const items = [];
  try {
    for (const c of (await App.GetContacts()) || []) {
      const conv = {
        id: 'd:' + c.peer_id, kind: 'direct', name: c.alias || c.peer_id.slice(0, 8),
        peerId: c.peer_id, pubKey: c.pub_key, online: !!c.is_online, last: '', ts: 0, unread: 0,
      };
      try {
        const hist = (await App.GetChatHistory(c.peer_id, 1, 0)) || [];
        if (hist[0]) {
          const m = directMessage(hist[0]);
          conv.last = preview(m);
          conv.ts = m.ts;
        }
      } catch (_) { /* no history yet */ }
      items.push(conv);
    }
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadContacts')));
  }
  try {
    for (const g of (await App.ListGroups()) || []) {
      let last = null;
      try {
        const recent = (await App.GetGroupMessages(g.id, 1)) || [];
        if (recent[0]) last = groupMessage(recent[0]);
      } catch (_) { /* nothing yet */ }
      items.push({
        id: 'g:' + g.id, kind: 'group', name: g.title, groupId: g.id, members: g.members, myRole: g.myRole,
        readOnly: g.readOnly, groupKind: g.kind, last: last ? last.text : '', ts: last ? last.ts : g.createdAt || 0, unread: 0,
      });
    }
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadGroups')));
  }
  const prev = Object.fromEntries(get(chats).map((c) => [c.id, c]));
  chats.set(items.map((c) => ({ ...c, unread: prev[c.id]?.unread || 0 })));
  listen();
}

function listen() {
  if (listening) return;
  listening = true;

  on('new_message', (msg) => {
    if (!msg) return;
    const partner = msg.is_mine ? msg.target_id : msg.sender_id;
    const id = 'd:' + partner;
    const conv = get(chats).find((c) => c.id === id);
    if (!conv) { loadChats(); return; } // a stranger wrote first: they become a contact
    const m = directMessage(msg);
    if (!m.mine && !m.system) play('receive', { quiet: inCall() });
    const open = get(activeId) === id;
    patch(id, (c) => ({ ...c, last: preview(m), ts: m.ts, unread: open || m.mine ? c.unread : c.unread + 1 }));
    if (open) messages.update((list) => (list.some((x) => x.id === m.id) ? list : [...list, m]));
  });

  on('group:event', async (ev) => {
    if (!ev) return;
    const id = 'g:' + ev.groupId;
    switch (ev.kind) {
      case 'message': {
        const open = get(activeId) === id;
        if (!get(chats).some((c) => c.id === id)) { await loadChats(); }
        play('receive', { quiet: inCall() });
        if (open) await reload();
        else patch(id, (c) => ({ ...c, unread: c.unread + 1 }));
        await refreshLast(id);
        break;
      }
      case 'members':
      case 'added':
        await loadChats();
        membersVersion.update((n) => n + 1);
        break;
      case 'removed':
        if (get(activeId) === id) { activeId.set(''); messages.set([]); }
        await loadChats();
        if (ev.reason === 'deleted') chatNote.set(ev.title ? tr('chats.note.deleted', { title: ev.title }) : tr('chats.note.groupDeleted'));
        break;
      case 'history':
        if (get(activeId) === id) await reload();
        await refreshLast(id);
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

  // messages that waited for the recipient get confirmed in the background: refresh the marks
  setInterval(() => {
    const open = get(activeId);
    if (open && get(messages).some((m) => m.pending)) reload();
  }, 5000);
}

function nameOfPeer(peerId) {
  const c = get(chats).find((x) => x.peerId === peerId);
  return c ? c.name : (peerId || '').slice(0, 8);
}

async function refreshLast(id) {
  const conv = get(chats).find((c) => c.id === id);
  if (!conv || conv.kind !== 'group') return;
  try {
    const recent = (await App.GetGroupMessages(conv.groupId, 1)) || [];
    if (recent[0]) patch(id, (c) => ({ ...c, last: recent[0].text, ts: recent[0].timestamp }));
  } catch (_) { /* keep as it was */ }
}

export async function openChat(id) {
  activeId.set(id);
  messages.set([]);
  chatNote.set('');
  patch(id, (c) => ({ ...c, unread: 0 }));
  await reload();
}

async function reload() {
  const conv = get(chats).find((c) => c.id === get(activeId));
  if (!conv) return;
  try {
    let list;
    if (conv.kind === 'direct') {
      list = ((await App.GetChatHistory(conv.peerId, 200, 0)) || []).map(directMessage).sort((a, b) => a.ts - b.ts);
    } else {
      list = ((await App.GetGroupMessages(conv.groupId, 200)) || []).map(groupMessage).sort((a, b) => a.ts - b.ts);
    }
    if (get(activeId) !== conv.id) return;
    messages.set(list);
    const last = list[list.length - 1];
    if (last) patch(conv.id, (c) => ({ ...c, last: preview(last), ts: last.ts }));
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.loadMessages')));
  }
}

export async function send(text) {
  text = text.trim();
  const conv = get(chats).find((c) => c.id === get(activeId));
  if (!text || !conv) return false;
  chatError.set('');
  chatNote.set('');
  try {
    if (conv.kind === 'direct') {
      if (!conv.pubKey) throw new Error(tr('chats.err.noKey'));
      await App.SendMessage(conv.pubKey, text);
    } else {
      const m = await App.SendGroupMessage(conv.groupId, text);
      if (m.queued > 0) chatNote.set(tr('chats.note.queued', { n: m.queued }));
      if (m.failed > 0) chatError.set(tr('chats.err.failed', { n: m.failed }));
    }
    play('send', { quiet: inCall() });
    await reload();
    return true;
  } catch (err) {
    chatError.set(errText(err, tr('chats.err.notSent')));
    return false;
  }
}

// a file posted in a group shows like a message we sent
onGroupPost(async (conv, m) => {
  if (m.queued > 0) chatNote.set(tr('chats.note.queued', { n: m.queued }));
  if (m.failed > 0) chatError.set(tr('chats.err.failed', { n: m.failed }));
  play('send', { quiet: inCall() });
  if (get(activeId) === conv.id) await reload();
  else patch(conv.id, (c) => ({ ...c, last: preview(groupMessage(m)), ts: m.timestamp }));
});

/** Adds a contact from a pasted/scanned link or key; name overrides the one in the link. */
export async function addContact(key, name) {
  const c = await App.AddContactByKey(key, name);
  await loadChats();
  await openChat('d:' + c.peer_id);
}

export async function deleteContact(conv) {
  await App.DeleteContact(conv.peerId);
  activeId.set('');
  messages.set([]);
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
  activeId.set('');
  messages.set([]);
  await loadChats();
}

export function closeChats() {
  activeId.set('');
  messages.set([]);
  chats.set([]);
  clearTransfers(); // another profile may log in next
  saved.set([]);
}
