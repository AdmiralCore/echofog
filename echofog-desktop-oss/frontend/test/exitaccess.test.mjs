import { test } from 'node:test';
import assert from 'node:assert/strict';
import proxy from '../src/lib/i18n/ns/proxy.js';

// T10.9: the texts of the desktop exit exist in both languages, and the warning says whose address
// the traffic leaves with.
test('every text of the exit access card has an English and a Russian version', () => {
  const keys = Object.keys(proxy.en).filter((k) => k.startsWith('proxy.xa.') || k === 'proxy.share.ipWarning');
  assert.ok(keys.length >= 20, 'the card has its texts');
  for (const k of keys) {
    assert.ok(proxy.ru[k], `ru ${k}`);
    assert.ok(proxy.en[k], `en ${k}`);
  }
  for (const k of Object.keys(proxy.ru).filter((k) => k.startsWith('proxy.xa.'))) assert.ok(proxy.en[k], `en ${k}`);
});

test('the warning names the IP address in both languages', () => {
  assert.match(proxy.en['proxy.share.ipWarning'], /IP address/);
  assert.match(proxy.ru['proxy.share.ipWarning'], /IP-адрес/);
});

test('placeholders agree between the languages', () => {
  const holes = (s) => (s.match(/\{[a-z]+\}/g) || []).sort().join(',');
  for (const k of Object.keys(proxy.en).filter((k) => k.startsWith('proxy.xa.'))) {
    assert.equal(holes(proxy.ru[k]), holes(proxy.en[k]), k);
  }
});

// Russian texts address nobody with an imperative of the second person: neutral form (rule 11).
test('Russian texts of the exit are in a neutral form', () => {
  const bad = /(?:^|[\s.,:;(])(?:подключитесь|отключите|включите|нажмите|выберите|введите|откройте|закройте|подтвердите|отклоните|убедитесь)(?=[\s.,:;)!?]|$)/i;
  for (const k of Object.keys(proxy.ru).filter((k) => k.startsWith('proxy.xa.') || k === 'proxy.share.ipWarning')) {
    assert.doesNotMatch(proxy.ru[k], bad, k);
  }
});
