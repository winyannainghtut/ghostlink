// The sign-in card takes a pasted login link, or the token alone (dashboard/src/loginLink.ts), code
// review 2026-11-25, U13: with no #t= in the address (or an expired one) there was nowhere to paste
// it. The token below is made up. Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseLoginLink } from '../dashboard/src/loginLink.ts';

const token = 'Zm9vYmFyYmF6cXV4MTIzNA';

test('the whole link, as the server prints it, gives its token', () => {
  assert.equal(parseLoginLink(`https://203.0.113.5:8443/#t=${token}`), token);
  assert.equal(parseLoginLink(`http://127.0.0.1:8080/#t=${token}`), token);
  assert.equal(parseLoginLink(`  https://vpn.example.com:8443/?x=1#t=${token}\n`), token);
  assert.equal(parseLoginLink(`https://vpn.example.com:8443/#other=1&t=${token}`), token);
});

test('the part after the # or the token alone also works', () => {
  assert.equal(parseLoginLink(`#t=${token}`), token);
  assert.equal(parseLoginLink(`t=${token}`), token);
  assert.equal(parseLoginLink(token), token);
  assert.equal(parseLoginLink(`  ${token}  `), token);
});

test('anything else is refused, not sent to the server', () => {
  for (const text of ['', '   ', 'hello', 'https://203.0.113.5:8443/', 'https://203.0.113.5:8443/#', 'https://203.0.113.5:8443/#t=', 't=short', 'a b c d e f g h i j k l m n o p', `${token}!!`, 'x'.repeat(200)]) {
    assert.equal(parseLoginLink(text), null, JSON.stringify(text));
  }
});
