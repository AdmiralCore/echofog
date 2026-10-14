import { writable } from 'svelte/store';
import { GetContacts, AddContact, DeleteContact } from '@wailsjs/go/main/App';

export const contacts = writable([]);
export const contactsLoading = writable(false);
export const contactsError = writable('');

function errText(err, fallback) {
    return (err && (err.message || String(err))) || fallback;
}

export async function loadContacts() {
    contactsLoading.set(true);
    contactsError.set('');
    try {
        const list = (await GetContacts()) || [];
        list.sort((a, b) => (a.alias || '').localeCompare(b.alias || ''));
        contacts.set(list);
    } catch (err) {
        contactsError.set(errText(err, 'Could not load contacts'));
    } finally {
        contactsLoading.set(false);
    }
}

export async function addContact(contact) {
    await AddContact(contact);
    await loadContacts();
}

export async function removeContact(peerId) {
    await DeleteContact(peerId);
    await loadContacts();
}
