// What is being written in each chat: the text, and the answer or the edit under way. It survives
// switching to another chat and back (for the life of the window).

const drafts = new Map();
const EMPTY = { text: '', replyTo: null, editing: null };

/** The draft of a chat (or of a thread: key '<chat id>#<root id>'). */
export function draftOf(key) {
  return drafts.get(key) || EMPTY;
}

/** Keeps the draft of a chat; an empty one is forgotten. */
export function keepDraft(key, d) {
  if (!key) return;
  if (!d.text && !d.replyTo && !d.editing) drafts.delete(key);
  else drafts.set(key, { text: d.text || '', replyTo: d.replyTo || null, editing: d.editing || null });
  if (drafts.size > 200) drafts.delete(drafts.keys().next().value);
}
