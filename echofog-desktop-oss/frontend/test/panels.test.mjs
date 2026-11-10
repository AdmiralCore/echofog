import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { get } from 'svelte/store';
import { panelHidden, togglePanel, selfCards, setSelfCard, personalCardOnScreen, spaceCardOnScreen, railCardShown } from '../src/lib/stores/panels.js';

const src = (file) => readFileSync(new URL('../src/lib/' + file, import.meta.url), 'utf8');
const count = (text, needle) => text.split(needle).length - 1;

// the "you" card stands at the foot of the personal list; the rail holds one too, and two of them
// on screen at once would say the same thing twice
test('a personal card is on screen only while the personal list is in place', () => {
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
    assert.equal(personalCardOnScreen(view, showAside, hidden), want, `${view} ${showAside} ${hidden}`);
  }
});

// a space's column of channels carries its own "you" card too, so while it is on screen the rail
// keeps its card away as well
test('a space card is on screen only while a space shows its column', () => {
  const cases = [
    ['chats', 'group', true, true], // the channels of a space are in place
    ['chats', 'direct', true, false], // a person has no column of channels
    ['chats', 'group', false, false], // folded to a strip, or the stage takes the whole window
    ['proxy', 'group', true, false], // another section: no chat on screen
    ['settings', 'group', true, false],
  ];
  for (const [view, kind, columnShown, want] of cases) {
    assert.equal(spaceCardOnScreen(view, kind, columnShown), want, `${view} ${kind} ${columnShown}`);
  }
});

test('the rail keeps its own card until a column reports one', () => {
  assert.equal(railCardShown({ personal: true, space: false }), false, 'the personal list');
  assert.equal(railCardShown({ personal: false, space: true }), false, 'a space');
  assert.equal(railCardShown({ personal: false, space: false }), true);
});

test('a column reports its own card without touching the other', () => {
  assert.deepEqual(get(selfCards), { personal: false, space: false });
  setSelfCard('personal', true);
  assert.deepEqual(get(selfCards), { personal: true, space: false });
  const reported = get(selfCards);
  setSelfCard('personal', true);
  assert.equal(get(selfCards), reported, 'the same report does not wake the rail');
  setSelfCard('space', true);
  assert.deepEqual(get(selfCards), { personal: true, space: true });
  setSelfCard('personal', false);
  assert.deepEqual(get(selfCards), { personal: false, space: true }, 'the space keeps its card');
  setSelfCard('space', false);
  assert.deepEqual(get(selfCards), { personal: false, space: false });
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
