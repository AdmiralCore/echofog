import { getContext, setContext } from 'svelte';

// The open chat's shared actions (answer, edit, open a thread or the menu, go to a message, names),
// set by views/Conversation.svelte for the parts of the screen in features/chat.
const KEY = Symbol('chat');

export const provideChat = (ctx) => setContext(KEY, ctx);
export const useChat = () => getContext(KEY);

/** Consecutive messages from one person within a few minutes form one visual group. */
const GROUP_GAP = 5 * 60 * 1000;
export const sameGroup = (a, b) => a && b && !a.system && !b.system && a.mine === b.mine && a.sender === b.sender && Math.abs((b.ts || 0) - (a.ts || 0)) < GROUP_GAP;

export const dayKey = (ts) => new Date(ts || 0).toDateString();

/** How far a message of mine got, as the screens name it. */
export const stateOf = (m) => (m.failed ? 'notSent' : m.pending ? m.state || 'waiting' : 'delivered');
