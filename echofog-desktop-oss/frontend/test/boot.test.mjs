import { test } from 'node:test';
import assert from 'node:assert/strict';
import boot from '../src/lib/i18n/ns/boot.js';

// T5.6: the entry-nodes screen has every text in both languages, and the v2 format hint names the
// forms a user can paste.
test('every entry-nodes text has an English and a Russian version', () => {
  for (const k of Object.keys(boot.ru).filter((k) => k.startsWith('boot.'))) {
    assert.ok(boot.en[k], `en ${k}`);
  }
  for (const k of Object.keys(boot.en).filter((k) => k.startsWith('boot.'))) {
    assert.ok(boot.ru[k], `ru ${k}`);
  }
});

test('the screen names the check, the switch-off and the v2 list forms', () => {
  assert.equal(boot.ru['boot.title'], 'Узлы входа');
  assert.ok(boot.en['boot.check']);
  assert.ok(boot.ru['boot.check']);
  assert.ok(boot.en['boot.disabled']);
  assert.match(boot.en['boot.formatV2'], /echofog:\/\/join\?v=2&card=/);
  assert.match(boot.en['boot.formatV2'], /reality2-tcp:\/\//);
  assert.match(boot.en['boot.formatV2'], /noise3:\/\//);
  assert.match(boot.ru['boot.formatV2'], /noise3:\/\//);
});
