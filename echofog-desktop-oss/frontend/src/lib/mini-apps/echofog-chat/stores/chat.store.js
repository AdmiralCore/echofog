import { writable, get } from 'svelte/store';
import { EventsOn } from '@wailsjs/runtime/runtime';
import { sendEncryptedMessage as coreSendEncryptedMessage } from '../../../stores/chat.store.js';
import {
    GetContacts, GetChatHistory, SendMessage, IssueVoucher, AcceptVoucher,
    GetPeerBalance, ClaimPeerVoucher,
} from '@wailsjs/go/main/App';

// Central store for the EchoFog Chat state
export const activeNexusId = writable("n_home");
export const activeChannelId = writable("");
export const rightPanelVisible = writable(false);
export const aiCommanderDragging = writable(false);
export const activeFilter = writable("all"); // 'all' | 'direct' | 'channel' | 'bot'

// Voice and PiP Stream State
export const isVoiceConnected = writable(false);
export const activeVoiceChannelId = writable(null);
export const micMuted = writable(false);
export const camMuted = writable(true);
export const screenSharing = writable(false);
export const activeStreamUser = writable(null); // { name, avatar, streamUrl }

// Telegram-Style Conversations Store
export const conversations = writable([]);

export const chatError = writable("");
export const peerBalances = writable({}); // { peerId: number }

const VOUCHER_PREFIX = "fogvoucher:";

function fmtTime(ts) {
    const ms = ts && ts < 1e12 ? ts * 1000 : ts;
    const d = ms ? new Date(ms) : new Date();
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function errText(err, fallback) {
    return (err && (err.message || String(err))) || fallback;
}

function mapMessage(m, contactAlias) {
    const text = m.text || "";
    const ms = m.timestamp && m.timestamp < 1e12 ? m.timestamp * 1000 : m.timestamp;
    const base = {
        id: m.id || ("m_" + m.timestamp),
        sender: m.is_mine ? "Me" : contactAlias,
        senderId: m.is_mine ? "" : (m.sender_id || ""),
        ts: ms || Date.now(),
        time: fmtTime(m.timestamp),
        isMine: !!m.is_mine,
        status: "sent",
    };
    if (text.startsWith(VOUCHER_PREFIX)) {
        try {
            const raw = text.slice(VOUCHER_PREFIX.length);
            const v = JSON.parse(raw);
            return {
                ...base,
                type: "voucher",
                text: "Voucher",
                voucher: { id: String(v.nonce), amount: v.amount, raw, memo: "", claimed: false },
            };
        } catch (_) { /* fall through to plain text */ }
    }
    return { ...base, type: "text", text };
}

async function loadHistoryFor(conv) {
    const history = (await GetChatHistory(conv.peerId, 50, 0)) || [];
    return history.slice().reverse().map((m) => mapMessage(m, conv.name));
}

function patchConversation(id, fn) {
    conversations.update((items) => items.map((c) => (c.id === id ? fn(c) : c)));
}

let listening = false;

export async function loadDirectConversations() {
    try {
        const list = (await GetContacts()) || [];
        const directs = [];
        for (const c of list) {
            const conv = {
                id: "d_" + c.peer_id,
                name: c.alias || c.peer_id.slice(0, 8),
                type: "direct",
                avatar: c.alias ? c.alias.charAt(0).toUpperCase() : "?",
                peerId: c.peer_id,
                pubKey: c.pub_key,
                online: !!c.is_online,
                statusText: "",
                lastMessage: "",
                time: "",
                unread: 0,
                messages: [],
            };
            try {
                conv.messages = await loadHistoryFor(conv);
                const last = conv.messages[conv.messages.length - 1];
                if (last) {
                    conv.lastMessage = last.type === "voucher" ? "Voucher" : last.text;
                    conv.time = last.time;
                }
            } catch (_) { /* history unavailable for this contact */ }
            directs.push(conv);
        }
        conversations.update((items) => [...directs, ...items.filter((c) => c.type !== "direct")]);
        const cur = get(activeChannelId);
        if (!get(conversations).some((c) => c.id === cur)) {
            const first = get(conversations)[0];
            if (first) activeChannelId.set(first.id);
        }
        directs.forEach((c) => refreshPeerBalance(c.peerId));
        chatError.set("");
    } catch (err) {
        chatError.set(errText(err, "Could not load chats"));
    }

    if (!listening) {
        listening = true;
        EventsOn("new_message", (msg) => {
            if (!msg) return;
            const partner = msg.is_mine ? msg.target_id : msg.sender_id;
            const conv = get(conversations).find((c) => c.peerId === partner);
            if (!conv) return;
            const mapped = mapMessage(msg, conv.name);
            patchConversation(conv.id, (c) => {
                if (c.messages.some((m) => m.id === mapped.id)) return c;
                return {
                    ...c,
                    lastMessage: mapped.type === "voucher" ? "Voucher" : mapped.text,
                    time: mapped.time,
                    messages: [...c.messages, mapped],
                };
            });
        });
    }
}

export async function refreshPeerBalance(peerId) {
    try {
        const bal = await GetPeerBalance(peerId);
        peerBalances.update((m) => ({ ...m, [peerId]: bal }));
    } catch (_) {
        peerBalances.update((m) => { const n = { ...m }; delete n[peerId]; return n; });
    }
}

async function reloadConversation(chatId) {
    const conv = get(conversations).find((c) => c.id === chatId);
    if (!conv || !conv.peerId) return;
    const messages = await loadHistoryFor(conv);
    const last = messages[messages.length - 1];
    patchConversation(chatId, (c) => ({
        ...c,
        messages,
        lastMessage: last ? (last.type === "voucher" ? "Voucher" : last.text) : c.lastMessage,
        time: last ? last.time : c.time,
    }));
}

export async function sendMessage(chatId, text) {
    if (!text || !text.trim()) return;
    const conv = get(conversations).find((c) => c.id === chatId);
    if (!conv) return;
    if (!conv.pubKey) {
        chatError.set("This conversation cannot send messages from here.");
        return;
    }
    try {
        await SendMessage(conv.pubKey, text.trim());
        chatError.set("");
        await reloadConversation(chatId);
    } catch (err) {
        chatError.set(errText(err, "Message was not sent"));
    }
}

export async function sendMicroVoucher(chatId, amount, recipientWallet) {
    const conv = get(conversations).find((c) => c.id === chatId);
    if (!conv || !conv.pubKey) {
        chatError.set("Pick a person to send tokens to.");
        return false;
    }
    try {
        await IssueVoucher(conv.pubKey, recipientWallet, Number(amount));
        chatError.set("");
        await reloadConversation(chatId);
        return true;
    } catch (err) {
        chatError.set(errText(err, "Voucher was not sent"));
        return false;
    }
}

export async function claimVoucher(chatId, voucherId) {
    const conv = get(conversations).find((c) => c.id === chatId);
    const msg = conv?.messages.find((m) => m.type === "voucher" && m.voucher?.id === voucherId);
    if (!conv || !msg) return;
    try {
        await AcceptVoucher(conv.peerId, msg.voucher.raw);
        await refreshPeerBalance(conv.peerId);
        await ClaimPeerVoucher(conv.peerId);
        patchConversation(chatId, (c) => ({
            ...c,
            messages: c.messages.map((m) => (m === msg ? { ...m, voucher: { ...m.voucher, claimed: true } } : m)),
        }));
        chatError.set("");
        await refreshPeerBalance(conv.peerId);
    } catch (err) {
        chatError.set(errText(err, "Voucher could not be claimed"));
    }
}
