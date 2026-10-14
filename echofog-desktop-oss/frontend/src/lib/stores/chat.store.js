import { writable, get } from 'svelte/store';
import { EventsOn } from '@wailsjs/runtime/runtime';
import { GetChatHistory, SendMessage } from '@wailsjs/go/main/App';

export const activeChatID = writable(null);
export const chatHistories = writable({}); // { "peer1_hex": [msg1, msg2], ... }

export const initChatListeners = () => {
    EventsOn("new_message", (msg) => {
        chatHistories.update(histories => {
            // Определяем, в какую "папку" положить сообщение
            const chatPartner = msg.is_mine ? msg.target_id : msg.sender_id;

            if (!histories[chatPartner]) {
                histories[chatPartner] = [];
            }
            // Добавляем сообщение в конец
            histories[chatPartner].push(msg);
            return histories;
        });
    });
};

// Загрузка истории при клике на контакт
export const loadHistory = async (peerID) => {
    activeChatID.set(peerID);

    // Если история уже загружена, не дергаем БД (оптимизация)
    const currentHistories = get(chatHistories);
    if (currentHistories[peerID]) return;

    try {
        // limit: 50, beforeTimestamp: 0 (самые свежие)
        const history = await GetChatHistory(peerID, 50, 0);
        chatHistories.update(h => {
            h[peerID] = history ? history.reverse() : []; // reverse, чтобы старые были сверху
            return h;
        });
    } catch (e) {
        console.error("History load error:", e);
    }
};

export const sendEncryptedMessage = async (text) => {
    const target = get(activeChatID);
    if (!target || !text.trim()) return;

    try {
        await SendMessage(target, text);
        // Само сообщение вернется нам через событие "new_message" с флагом is_mine: true,
        // поэтому мы НЕ добавляем его в UI вручную. Это гарантирует консистентность БД и UI.
    } catch (e) {
        console.error("Send error:", e);
    }
};