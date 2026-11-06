import { test } from 'node:test';
import assert from 'node:assert/strict';
import { classifyLink, fromWebLink, looksLikeLink, webLinkOf, joinInfo, MAX_LINK } from '../src/lib/links.js';

const CARD = 'A'.repeat(300);
const V2 = `echofog://join?v=2&card=${CARD}`;
const V2_INVITE = `echofog://join?v=2&card=${CARD}&s=${'B'.repeat(43)}`;

test('a v2 contact link is a join link of version 2 without a secret', () => {
  assert.equal(classifyLink(V2)?.kind, 'join');
  assert.deepEqual(joinInfo(V2), { version: 2, hasSecret: false });
});

test('a v2 invitation carries a secret', () => {
  assert.equal(classifyLink(V2_INVITE)?.kind, 'join');
  assert.deepEqual(joinInfo(V2_INVITE), { version: 2, hasSecret: true });
});

test('the slash form of the scheme is the same link', () => {
  const slash = `echofog://join/?v=2&card=${CARD}`;
  assert.equal(classifyLink(slash)?.text, V2);
  assert.deepEqual(joinInfo(slash), { version: 2, hasSecret: false });
});

test('the https page form opens the same link', () => {
  const web = `https://join.example.org/join/#join?v=2&card=${CARD}&s=XYZ`;
  const c = classifyLink(web);
  assert.equal(c?.kind, 'join');
  assert.equal(c.text, `echofog://join?v=2&card=${CARD}&s=XYZ`);
  assert.deepEqual(joinInfo(web), { version: 2, hasSecret: true });
  // percent-encoded on the way
  assert.equal(classifyLink(`https://h/join/#${encodeURIComponent(V2)}`)?.kind, 'join');
});

test('a link inside words and with trailing punctuation', () => {
  assert.equal(fromWebLink(`Join me: ${V2}.`), V2);
  assert.equal(fromWebLink(`(${V2_INVITE}), please`), V2_INVITE);
  assert.ok(looksLikeLink(`see ${V2}`));
});

test('a v1 join link is version 1', () => {
  assert.deepEqual(joinInfo('echofog://join?id=' + 'a'.repeat(64) + '&addr=1.2.3.4:5'), { version: 1, hasSecret: false });
  assert.deepEqual(joinInfo('echofog://join?id=' + 'a'.repeat(64) + '&token=abc'), { version: 1, hasSecret: true });
});

test('a link of a version this build does not know is reported as such', () => {
  assert.deepEqual(joinInfo(`echofog://join?v=3&card=${CARD}`), { version: 3, hasSecret: false });
  assert.deepEqual(joinInfo(`echofog://join?v=x&card=${CARD}`), { version: 0, hasSecret: false });
});

test('other text has no join info', () => {
  for (const s of ['', '   ', 'hello', 'echofog://relay/1.2.3.4:5/' + 'a'.repeat(64), 'fogroup2:abc', null, undefined, 42]) {
    assert.equal(joinInfo(s), null, String(s));
  }
});

test('a link above the size limit is not a link', () => {
  assert.equal(MAX_LINK, 16 * 1024);
  const huge = `echofog://join?v=2&card=${'A'.repeat(MAX_LINK)}`;
  assert.equal(classifyLink(huge), null);
  assert.equal(joinInfo(huge), null);
  assert.equal(looksLikeLink(huge), false);
  const fits = `echofog://join?v=2&card=${'A'.repeat(MAX_LINK - 'echofog://join?v=2&card='.length)}`;
  assert.equal(classifyLink(fits)?.kind, 'join');
});

test('the https form of a link is made from the app form', () => {
  assert.ok(webLinkOf(V2).endsWith('#join?v=2&card=' + CARD));
});

// A parser that takes text from anywhere must never throw and never return an unbounded value.
test('random text never breaks the parsers', () => {
  let seed = 12345;
  const rnd = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n; };
  const pieces = ['echofog://', 'join', '/?', '?', '&', 'v=2', 'card=', 's=', 'https://', '#', '%', '%3A', '%zz', ' ', '\n', '\u0000', 'fogroup2:', 'fogcontact1:', 'relay/', 'A', '=', '.', ')', '»', '‮', '😀'];
  for (let i = 0; i < 20000; i++) {
    let s = '';
    for (let k = rnd(12); k >= 0; k--) s += pieces[rnd(pieces.length)];
    if (rnd(50) === 0) s += 'A'.repeat(rnd(20000));
    const c = classifyLink(s);
    const j = joinInfo(s);
    looksLikeLink(s);
    fromWebLink(s);
    if (c) {
      assert.ok(c.text.length <= MAX_LINK, 'bounded');
      assert.ok(['join', 'node', 'contact', 'group'].includes(c.kind));
    }
    if (j) assert.ok(Number.isInteger(j.version) && j.version >= 0);
  }
});
