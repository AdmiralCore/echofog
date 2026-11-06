import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SCREEN_CODES, NOTICE_KINDS, screenKeys, noticeKey } from '../src/lib/errscreen.js';
import errscreen from '../src/lib/i18n/ns/errscreen.js';
import shell from '../src/lib/i18n/ns/shell.js';

const all = (lang) => ({ ...errscreen[lang], ...shell[lang] });

test('the three error screens and the notices are known', () => {
  assert.deepEqual([...SCREEN_CODES].sort(), ['free-quota-used', 'no-cover', 'other-version']);
  assert.deepEqual([...NOTICE_KINDS].sort(), ['clock-skew', 'no-cover', 'no-domestic-entry', 'release-unavailable', 'update']);
  assert.equal(screenKeys('nonsense'), null);
  assert.equal(screenKeys(undefined), null);
  assert.equal(noticeKey('nonsense'), '');
});

test('every screen and notice has English and Russian text', () => {
  for (const lang of ['en', 'ru']) {
    const d = all(lang);
    for (const code of SCREEN_CODES) {
      const k = screenKeys(code);
      assert.ok(d[k.title] && d[k.text], `${lang} ${code}`);
    }
    for (const kind of NOTICE_KINDS) assert.ok(d[noticeKey(kind)], `${lang} notice ${kind}`);
    for (const k of ['update.important', 'update.notes', 'update.textV2', 'errscreen.close']) assert.ok(d[k], `${lang} ${k}`);
  }
});

test('Russian texts are in a neutral form', () => {
  const d = all('ru');
  const mine = Object.entries(d).filter(([k]) => /^(errscreen\.|notice\.|update\.(important|notes|textV2))/.test(k));
  assert.ok(mine.length >= 15);
  for (const [k, v] of mine) {
    assert.match(v, /[А-Яа-яЁё]/, k);
    assert.doesNotMatch(v, /(^|[^А-Яа-яЁё])(вы|Вы|ваш[а-я]*|Ваш[а-я]*|сделал[а-я]*|нажал[а-я]*|ввёл|ввела)([^А-Яа-яЁё]|$)/, k);
  }
});

test('the texts do not promise an automatic download or install', () => {
  for (const lang of ['en', 'ru']) {
    const d = all(lang);
    for (const [k, v] of Object.entries(d)) {
      if (/^update\.(important|notes|textV2)$/.test(k)) assert.doesNotMatch(v, /automatic|автоматическ/i, k);
    }
  }
});
