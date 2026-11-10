import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { get } from 'svelte/store';
import { panelHidden, togglePanel, selfCardShown, selfCardOnScreen } from '../src/lib/stores/panels.js';

const src = (file) => readFileSync(new URL('../src/lib/' + file, import.meta.url), 'utf8');
const count = (text, needle) => text.split(needle).length - 1;

// the "you" card stands at the foot of the personal list; the rail holds one too, and two of them
// on screen at once would say the same thing twice
test('a self card is on screen only while the personal list is in place', () => {
  const cases = [
    ['chats', true, false, true], // the list of chats is in place
    ['chats', false, false, false], // a space shows its channels instead
    ['chats', true, true, false], // the column is folded to a strip
    ['chats', false, true, false],
    ['proxy', true, false, false], // another section: no chat list on screen
    ['wallet', true, true, false],
    ['settings', true, false, false],
  ];
  for (const [view, showAside, hidden, want] of cases) {
    assert.equal(selfCardOnScreen(view, showAside, hidden), want, `${view} ${showAside} ${hidden}`);
  }
});

test('the rail keeps its own card until a column reports one', () => {
  assert.equal(get(selfCardShown), false);
});

test('the two columns fold apart, each on its own', () => {
  const personal = get(panelHidden.personal);
  const space = get(panelHidden.space);
  togglePanel('personal');
  assert.equal(get(panelHidden.personal), !personal);
  assert.equal(get(panelHidden.space), space, 'a space keeps its channel column');
  togglePanel('space');
  assert.equal(get(panelHidden.space), !space);
  assert.equal(get(panelHidden.personal), !personal, 'the personal list stays folded');
  togglePanel('personal');
  togglePanel('space');
  assert.equal(get(panelHidden.personal), personal);
  assert.equal(get(panelHidden.space), space);
});

// one switch per column, in the column it hides: the head of the personal list and the head of the
// channels; the chat's own head carries none (its "channels" button of a narrow window is another one)
test('one side-panel switch per column, inside that column', () => {
  assert.equal(count(src('views/Chats.svelte'), 'icon="panel"'), 1, 'Chats');
  assert.equal(count(src('features/chat/ChannelBar.svelte'), 'icon="panel"'), 1, 'ChannelBar');
  assert.equal(count(src('features/chat/ChatHeader.svelte'), 'icon="panel"'), 0, 'ChatHeader');
  assert.equal(count(src('views/Saved.svelte'), 'icon="panel"'), 0, 'Saved');
  for (const file of ['features/chat/ChatHeader.svelte', 'views/Saved.svelte']) {
    assert.equal(count(src(file), 'togglePanel('), 0, file);
  }
});
