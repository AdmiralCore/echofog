import { writable } from 'svelte/store';
import { GetContacts, AddContact } from '@wailsjs/go/main/App';

// Храним в виде объекта для мгновенного поиска: { "peer_id_123": { alias: "Bob", ... } }
export const contactsList = writable({});

// Вызывай это один раз после старта ноды (например, в bootCoreNode)
export const loadContacts = async () => {
    try {
        const contactsArr = await GetContacts();
        if (contactsArr) {
            const map = {};
            contactsArr.forEach(c => map[c.peer_id] = c);
            contactsList.set(map);
        }
    } catch (err) {
        console.error("Failed to load contacts:", err);
    }
};

// Функция добавления/обновления
export const saveContact = async (peerID, alias) => {
    const newContact = {
        peer_id: peerID,
        alias: alias.trim()
    };

    try {
        await AddContact(newContact); // Отправляем в Go

        // Мгновенно обновляем интерфейс
        contactsList.update(curr => {
            curr[peerID] = newContact;
            return curr;
        });
        return true;
    } catch (err) {
        console.error("Failed to save contact:", err);
        return false;
    }
};