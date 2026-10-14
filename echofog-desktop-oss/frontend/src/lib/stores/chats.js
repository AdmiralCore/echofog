import { toMs } from '../format.js';
import { writable, get } from 'svelte/store';
import { App, on, errText } from '../api.js';
import { play } from '../sounds.js';
import { callState } from '../services/call.service.js';
import { ringIncoming, incomingCall, declineIncomingCall } from './call.store.js';

const inCall = () => get(callState).inCall;

const CALL_INVITE = 'fogcall://';

/** Conversations: { id, kind: 'direct' | 'group', name, peerId, pubKey, online, last, ts, unread, readOnly } */
export const chats = writable([]);
export const activeId = writable('');
/** Messages of the open conversation: { id, mine, sender, text, ts, failed, pending } */
export const messages = writable([]);
export const chatError = writable('');
/** A hint that is not an error, e.g. that members are offline and get the message later. */
export const chatNote = writable('');
/** Goes up whenever the member list or the roles of some group changed. */
export const membersVersion = writable(0);

let listening = false;

function directMessage(m) {
  const call = (m.text || '').startsWith(CALL_INVITE);
  return {
    id: m.id || 'm' + m.timestamp,
    mine: !!m.is_mine,
    sender: m.is_mine ? '' : m.sender_id,
    text: call ? (m.is_mine ? 'Outgoing call' : 'Incoming call') : m.text,
    system: call,
    pending: !!m.pending,
    ts: toMs(m.timestamp),
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
          conv.last = m.text;
          conv.ts = m.ts;
        }
      } catch (_) { /* no history yet */ }
      items.push(conv);
    }
  } catch (err) {
    chatError.set(errText(err, 'Could not load contacts'));
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
    chatError.set(errText(err, 'Could not load groups'));
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
    patch(id, (c) => ({ ...c, last: m.text, ts: m.ts, unread: open || m.mine ? c.unread : c.unread + 1 }));
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
        if (ev.reason === 'deleted') chatNote.set(`"${ev.title || 'The group'}" was deleted by its owner.`);
        break;
      case 'history':
        if (get(activeId) === id) await reload();
        await refreshLast(id);
        break;
      case 'callend':
        // the call is over: if it was still ringing here, stop
        if (get(incomingCall)?.callId === ev.callId) {
          declineIncomingCall();
          chatNote.set(`Missed call in ${ev.title || 'the group'}.`);
        }
        break;
      case 'call':
        // somebody started a call in a group we are in
        ringIncoming({
          callId: ev.callId, coordinator: ev.coordinator, from: ev.from, chatId: id,
          alias: `${ev.title || 'Group'}: ${nameOfPeer(ev.from)}`,
        });
        break;
      default:
    }
  });

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
    if (last) patch(conv.id, (c) => ({ ...c, last: last.text, ts: last.ts }));
  } catch (err) {
    chatError.set(errText(err, 'Could not load messages'));
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
      if (!conv.pubKey) throw new Error('This contact has no key yet. Ask them to share their contact link.');
      await App.SendMessage(conv.pubKey, text);
    } else {
      const m = await App.SendGroupMessage(conv.groupId, text);
      if (m.queued > 0) chatNote.set(`${m.queued} member${m.queued === 1 ? ' is' : 's are'} offline and will get it when back.`);
      if (m.failed > 0) chatError.set(`Could not send to ${m.failed} member${m.failed === 1 ? '' : 's'}.`);
    }
    play('send', { quiet: inCall() });
    await reload();
    return true;
  } catch (err) {
    chatError.set(errText(err, 'Message was not sent'));
    return false;
  }
}

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
}
