import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createBrowserClient, SDKError} from './sdk/index.js';
const origin = 'https://console.example.test';
const csrf = 'a'.repeat(43);
const record = {id: 'r1', name: 'One', count: 0, enabled: false, description: null, server_id: 'server'};
const {server_id, ...requestRecord} = record;
const json = (body, status = 200, headers = {}) => new Response(JSON.stringify(body), {status, headers: {'Content-Type': 'application/json', ...headers}});
const session = () => json({authenticated: true, roles: ['user'], user: {sub: 'owner'}, expires_at: Date.now() + 60000, csrf_token: csrf});
function client(fetcher) { globalThis.location = {origin}; globalThis.fetch = fetcher; return createBrowserClient(); }
const error = (code, status = 0) => e => e instanceof SDKError && e.code === code && e.status === status && !e.message.includes('private');
test('request shape, session CSRF, JSON values, query encoding, and logout', async () => {
  const requests = [];
  const sdk = client(async (url, options) => {
    requests.push([url, options]);
    assert.equal(options.credentials, 'same-origin'); assert.equal(options.redirect, 'error'); assert.equal(options.mode, 'same-origin');
    assert.equal(options.headers.Authorization, undefined); assert.equal(options.headers.Cookie, undefined);
    if (url.endsWith('/auth/session')) return session();
    if (options.method !== 'GET') assert.equal(options.headers['X-CSRF-Token'], csrf);
    if (url.endsWith('/auth/logout') || options.method === 'DELETE') return new Response(null, {status: 204});
    if (options.method === 'POST') assert.deepEqual(JSON.parse(options.body), requestRecord);
    return json(record, options.method === 'POST' ? 201 : 200, {ETag: '"v1"'});
  });
  assert.equal((await sdk.createRecord({body: requestRecord})).body.enabled, false);
  assert.equal((await sdk.getRecord({id: 'r1', search: 'a & b'})).etag, '"v1"');
  assert.equal(requests[2][0], origin + '/api/records/r1?search=a+%26+b');
  const value = await sdk.session(); assert.equal(value.csrf_token, undefined);
  await sdk.deleteRecord({id: 'r1'}); await sdk.logout();
});
test('invalid input performs no request', async () => {
  let calls = 0; const sdk = client(async () => { calls++; return json(record); });
  for (const input of [{id: '..'}, {id: 'x', token: 'private'}, {id: '\ud800'}, {id: 'x'.repeat(9000)}]) await assert.rejects(sdk.getRecord(input), error('invalid_input'));
  for (const body of [record, {...requestRecord, count: 1.5}, {...requestRecord, count: Number.MAX_SAFE_INTEGER + 1}, {...requestRecord, name: ''}, {...requestRecord, private: 'hidden'}]) await assert.rejects(sdk.createRecord({body}), error('invalid_input'));
  await assert.rejects(sdk.createRecord({body: {...requestRecord, description: 'a'.repeat((1 << 20) + 1)}}), error('invalid_input'));
  assert.equal(calls, 0);
});
test('invalid responses and unsafe numbers are rejected', async () => {
  for (const body of ['{"server_id":"server","id":"r1","id":"r2","name":"One","count":0,"enabled":false}', '{"server_id":"server","id":"r1","name":"\\ud800","count":0,"enabled":false}', JSON.stringify({...record, secret: 'private'}), JSON.stringify({...record, count: -1}), JSON.stringify({...record, mode: null}), JSON.stringify({...record, endpoint: "https://invalid.example/%zz"}), JSON.stringify({...record, created_at: '2026-02-31T00:00:00Z'}), JSON.stringify({...record, count: Number.MAX_SAFE_INTEGER + 1}), '{"server_id":"server","id":"r1","name":"One","count":1.0000000000000001,"enabled":false}', '{}', JSON.stringify({...record, private: 'value'})]) {
    const sdk = client(async () => new Response(body, {headers: {'Content-Type': 'application/json'}}));
    await assert.rejects(sdk.getRecord({id: 'r1'}), error('invalid_response'));
  }
});
test('HTTP errors and exceptions do not expose private response data', async () => {
  for (const status of [401, 403, 404, 409, 503]) { const sdk = client(async () => json({private: 'private'}, status)); await assert.rejects(sdk.getRecord({id: 'r1'}), error(status === 401 ? 'reauth_required' : 'http_error', status)); }
  const sdk = client(async () => { throw new Error('private'); }); await assert.rejects(sdk.getRecord({id: 'r1'}), error('request_failed'));
});
test('response bytes, media type, and schema limits are enforced', async () => {
  const huge = client(async () => new Response(' '.repeat((4 << 20) + 1), {headers: {'Content-Type': 'application/json'}})); await assert.rejects(huge.getRecord({id: 'r1'}), error('response_limit'));
  const wrong = client(async () => new Response(JSON.stringify(record), {headers: {'Content-Type': 'application/jsonp'}})); await assert.rejects(wrong.getRecord({id: 'r1'}), error('invalid_response'));
});
test('cancellation and concurrency limits release capacity', async () => {
  const pending = []; const sdk = client((url, options) => new Promise((resolve, reject) => { pending.push(resolve); options.signal.addEventListener('abort', () => reject(new Error('private')), {once: true}); }));
  const controller = new AbortController(); controller.abort(); await assert.rejects(sdk.getRecord({id: 'r1'}, {signal: controller.signal}), error('cancelled')); assert.equal(pending.length, 0);
  const requests = Array.from({length: 16}, () => sdk.getRecord({id: 'r1'})); await assert.rejects(sdk.getRecord({id: 'r1'}), error('busy'));
  for (const resolve of pending) resolve(json(record)); await Promise.all(requests);
  const next = client(async () => json(record)); assert.equal((await next.getRecord({id: 'r1'})).status, 200);
});
test('an unauthenticated session prevents mutation and private session fields are rejected', async () => {
  let calls = 0; const sdk = client(async () => { calls++; return json({authenticated: false, roles: []}); }); await assert.rejects(sdk.createRecord({body: requestRecord}), error('reauth_required', 401)); assert.equal(calls, 1);
  const bad = client(async () => json({authenticated: false, roles: [], access_token: 'private'})); await assert.rejects(bad.session(), error('invalid_response'));
});
test('HTTPS is required', () => { globalThis.location = {origin: 'http://localhost'}; assert.throws(createBrowserClient, error('invalid_origin')); });

test('operation names do not alter object prototypes', async () => {
 const sdk=client(async () => json(record));
 assert.equal(Object.getPrototypeOf(sdk),null);
 assert.equal((await sdk['__proto__']()).body.id,'r1');
 assert.equal(Object.prototype.id,undefined);
});


test('in-flight cancellation and the whole-operation deadline release capacity', async () => {
  const sdk = client((url, options) => new Promise((resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new Error('private')), {once: true});
  }));
  const controller = new AbortController();
  const cancelled = sdk.getRecord({id: 'r1'}, {signal: controller.signal});
  controller.abort();
  await assert.rejects(cancelled, error('cancelled'));

  const originalTimer = globalThis.setTimeout;
  let expire;
  globalThis.setTimeout = (callback, delay, ...args) => {
    if (delay === 20000) { expire = callback; return originalTimer(() => {}, 90000); }
    return originalTimer(callback, delay, ...args);
  };
  try {
    const timedOut = sdk.getRecord({id: 'r1'});
    assert.equal(typeof expire, 'function');
    expire();
    await assert.rejects(timedOut, error('timeout'));
  } finally { globalThis.setTimeout = originalTimer; }
  const next = client(async () => json(record));
  assert.equal((await next.getRecord({id: 'r1'})).status, 200);
});


test('only declared API error codes leave the transport', async () => {
  for (const code of ['record_name_exists', 'private_provider_failure']) {
    const sdk = client(async () => json({code, reason: 'private', operation_id: 'private'}, 409));
    await assert.rejects(sdk.getRecord({id: 'r1'}), e => {
      assert.ok(error('http_error', 409)(e));
      assert.equal(e.apiCode, code === 'record_name_exists' ? code : undefined);
      assert.equal(e.operationId, undefined);
      assert.equal(e.reason, undefined);
      return true;
    });
  }
});
test('login uses a fixed same-origin path and bounds the return address', () => {
  const sdk = client(async () => json(record));
  let destination;
  globalThis.location = {origin, pathname: '/records/r1', search: '?tab=info', hash: '#heading', assign: value => { destination = value; }};
  sdk.login();
  const target = new URL(destination);
  assert.equal(target.origin, origin); assert.equal(target.pathname, '/auth/login');
  assert.equal(target.searchParams.get('return_to'), '/records/r1?tab=info#heading');
  globalThis.location.pathname = '//other.example/';
  sdk.login(); assert.equal(new URL(destination).searchParams.get('return_to'), '/');
});
