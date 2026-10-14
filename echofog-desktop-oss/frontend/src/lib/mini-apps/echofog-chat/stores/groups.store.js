import { writable, get } from 'svelte/store';
import {
    ListGroups, CreateGroup, JoinGroup, GetGroupInvite, AddGroupMember,
    ListGroupMembers, SendGroupMessage, GetGroupMessages,
} from '@wailsjs/go/main/App';
import { conversations, activeChannelId, chatError } from './chat.store.js';

export const groupMembers = writable([]);
export const groupInvite = writable("");

function errText(err, fallback) {
    return (err && (err.message || String(err))) || fallback;
}

function fmtTime(ts) {
    return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function mapMessage(m) {
    return {
        id: m.id,
        sender: m.isMine ? "Me" : m.sender.slice(0, 8),
        senderId: m.isMine ? "" : m.sender,
        ts: m.timestamp,
        text: m.text,
        time: fmtTime(m.timestamp),
        isMine: !!m.isMine,
        type: "text",
        status: m.failed > 0 && m.delivered === 0 ? "failed" : "sent",
    };
}

function mapGroup(g, messages) {
    const last = messages[messages.length - 1];
    return {
        id: g.id,
        name: g.title,
        type: "channel",
        kind: g.kind,
        avatar: g.title.charAt(0).toUpperCase(),
        peerId: g.id,
        online: false,
        readOnly: g.readOnly,
        myRole: g.myRole,
        membersCount: g.members,
        description: g.description,
        statusText: `${g.kind} - ${g.members} ${g.members === 1 ? "member" : "members"}`,
        lastMessage: last ? last.text : "",
        time: last ? last.time : "",
        unread: 0,
        messages,
    };
}

async function loadMessages(groupId) {
    try {
        return ((await GetGroupMessages(groupId, 100)) || []).map(mapMessage);
    } catch (_) {
        return [];
    }
}

export async function loadGroupConversations() {
    try {
        const list = (await ListGroups()) || [];
        const mapped = [];
        for (const g of list) {
            mapped.push(mapGroup(g, await loadMessages(g.id)));
        }
        conversations.update((items) => [...items.filter((c) => c.type !== "channel"), ...mapped]);
        const cur = get(activeChannelId);
        if (!cur) {
            const first = get(conversations)[0];
            if (first) activeChannelId.set(first.id);
        }
    } catch (err) {
        chatError.set(errText(err, "Could not load groups"));
    }
}

export async function createGroup(title, description, kind) {
    try {
        const g = await CreateGroup(title, description, kind);
        await loadGroupConversations();
        activeChannelId.set(g.id);
        chatError.set("");
        return g;
    } catch (err) {
        chatError.set(errText(err, "Group was not created"));
        return null;
    }
}

export async function joinGroup(invite) {
    try {
        const g = await JoinGroup(invite);
        await loadGroupConversations();
        activeChannelId.set(g.id);
        chatError.set("");
        return g;
    } catch (err) {
        chatError.set(errText(err, "Could not join this group"));
        return null;
    }
}

export async function sendGroupChatMessage(groupId, text) {
    if (!text || !text.trim()) return false;
    try {
        const m = await SendGroupMessage(groupId, text.trim());
        const mapped = mapMessage(m);
        conversations.update((items) => items.map((c) => (c.id === groupId
            ? { ...c, lastMessage: mapped.text, time: mapped.time, messages: [...c.messages, mapped] }
            : c)));
        chatError.set(m.failed > 0 ? `Not delivered to ${m.failed} member${m.failed === 1 ? "" : "s"}.` : "");
        return true;
    } catch (err) {
        chatError.set(errText(err, "Message was not sent"));
        return false;
    }
}

export async function loadGroupDetails(groupId) {
    groupInvite.set("");
    try {
        groupMembers.set((await ListGroupMembers(groupId)) || []);
        groupInvite.set(await GetGroupInvite(groupId));
    } catch (err) {
        groupMembers.set([]);
        chatError.set(errText(err, "Could not load group details"));
    }
}

export async function addGroupMember(groupId, pubKeyHex) {
    try {
        await AddGroupMember(groupId, pubKeyHex);
        await loadGroupConversations();
        await loadGroupDetails(groupId);
        chatError.set("");
        return true;
    } catch (err) {
        chatError.set(errText(err, "Member was not added"));
        return false;
    }
}
