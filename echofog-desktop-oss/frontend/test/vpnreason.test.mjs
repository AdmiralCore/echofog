import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EXIT_REASONS, EXIT_PROBLEMS, SITE_REASONS, refusalKey, historyKey, isSiteFailure } from '../src/lib/vpnreason.js';
import proxy from '../src/lib/i18n/ns/proxy.js';

// the stable codes of internal/v2/exit/reason.go (T10.4): a new reason goes at the end
test('the reason codes are the ones of the core', () => {
  assert.deepEqual(EXIT_REASONS, ['no-camouflage', 'unreachable', 'busy', 'policy', 'rate-limited', 'no-path']);
  assert.deepEqual([...EXIT_PROBLEMS].sort(), ['disabled', 'no-camouflage', 'quota', 'unreachable']);
  assert.deepEqual(SITE_REASONS, ['dial failed', 'unreachable']);
});

test('every reason has an English and a Russian text for the status, the history and the exit problem', () => {
  for (const lang of ['en', 'ru']) {
    const d = proxy[lang];
    for (const code of EXIT_REASONS) {
      assert.ok(d[refusalKey(code)], `${lang} ${refusalKey(code)}`);
      assert.ok(d[historyKey(code)], `${lang} ${historyKey(code)}`);
    }
    for (const state of EXIT_PROBLEMS) assert.ok(d['proxy.exit.' + state], `${lang} proxy.exit.${state}`);
    assert.ok(d['proxy.externalExit'], `${lang} external resource note`);
  }
});

test('an unknown reason has a text too, so the status never shows a raw key', () => {
  for (const lang of ['en', 'ru']) assert.ok(proxy[lang][refusalKey('something new')], lang);
  assert.equal(refusalKey('no-path'), 'proxy.refusal.no-path');
  assert.equal(refusalKey('dial failed'), 'proxy.refusal.dial failed');
  assert.equal(historyKey('rate-limited'), 'proxy.hist.why.rate-limited');
  assert.equal(historyKey('dial failed'), 'proxy.hist.why.dial_failed');
});

test('only the site-side failures are counted as the site, the rest points at the exit', () => {
  assert.equal(isSiteFailure('unreachable'), true);
  assert.equal(isSiteFailure('dial failed'), true);
  for (const code of ['no-path', 'no-camouflage', 'busy', 'policy', 'rate-limited', 'exit unreachable']) assert.equal(isSiteFailure(code), false, code);
});

test('Russian texts of the reasons are in a neutral form', () => {
  const d = proxy.ru;
  const keys = [...EXIT_REASONS.flatMap((c) => [refusalKey(c), historyKey(c)]), ...[...EXIT_PROBLEMS].map((s) => 'proxy.exit.' + s), 'proxy.externalExit'];
  for (const k of keys) {
    assert.match(d[k], /[А-Яа-яЁё]/, k);
    assert.doesNotMatch(d[k], /(^|[^А-Яа-яЁё])(вы|Вы|ваш[а-я]*|Ваш[а-я]*|сделал[а-я]*|нажал[а-я]*|ввёл|ввела)([^А-Яа-яЁё]|$)/, k);
  }
});
