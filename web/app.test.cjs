const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const host = 'panel.test:8317';
const originalUA = 'FixtureBrowser/1.0';
const fixtureKey = 'synthetic-key-for-remembered-login';
const remembered = JSON.stringify({state: {apiBase: 'http://' + host, managementKey: fixtureKey, rememberPassword: true}, version: 0});
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const readKey = new vm.Script(source.slice(source.indexOf('function xorBytes'), source.indexOf('function showConnectionNotice')) + '\nreadCPAKey();');

function storedValue(version, secret, value = remembered) {
  const keyBytes = Buffer.from(secret);
  const bytes = Buffer.from(value);
  for (let i = 0; i < bytes.length; i++) bytes[i] ^= keyBytes[i % keyBytes.length];
  return `enc::${version}::${bytes.toString('base64')}`;
}

function read(raw, userAgent = originalUA, storageHost = host) {
  return readKey.runInNewContext({
    atob, TextDecoder, TextEncoder,
    location: {host: storageHost},
    navigator: {userAgent},
    localStorage: {getItem(name) { assert.equal(name, 'cli-proxy-auth'); return raw; }},
  });
}

const v1 = storedValue('v1', 'cli-proxy-api-webui::secure-storage|panel.test:8317|FixtureBrowser/1.0');
const v2 = storedValue('v2', 'cli-proxy-api-webui::secure-storage|panel.test:8317');

test('v1 reads the remembered key with the original host and user agent', () => {
  assert.ok(read(v1) === fixtureKey, 'v1 fixture did not decode');
  assert.ok(read(v1, 'FixtureBrowser/2.0') !== fixtureKey, 'v1 must still depend on the original user agent');
});

test('v2 reads the remembered key with the original host and port regardless of user agent', () => {
  assert.ok(read(v2, 'FixtureBrowser/2.0') === fixtureKey, 'v2 fixture did not decode');
  assert.ok(read(v2, originalUA, 'panel.test:8318') !== fixtureKey, 'v2 must still depend on the original host and port');
});

test('existing plaintext fallback still works; missing and unusable entries yield no key', () => {
  assert.ok(read(remembered) === fixtureKey, 'plaintext fallback did not decode');
  for (const raw of [null, 'enc::v2::!', v2.replace('v2', 'v3'), JSON.stringify({state: {rememberPassword: false}})]) {
    assert.ok(read(raw) === '', 'unusable entry returned a key');
  }
});

async function mountPlugin(previewResponse) {
  const nodes = new Map();
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, {
      value: 'all', style: {}, handlers: {}, classList: {add() {}, remove() {}},
      setAttribute() {}, addEventListener(event, handler) { this.handlers[event] = handler; },
    });
    return nodes.get(id);
  };
  const provider = {value: 'codex'};
  const window = {};
  window.parent = window;
  const settings = {strategy: 'quota_bands', providers: ['codex'], interval_minutes: 15, failure_threshold: 3, antigravity_group: 'gemini'};
  const plan = {credentials: [], changes: 0};
  const json = payload => ({ok: true, status: 200, json: async () => payload});
  const calls = [];
  vm.runInNewContext(source, {
    window, atob, TextDecoder, TextEncoder, URLSearchParams,
    location: {host, search: ''}, navigator: {userAgent: 'FixtureBrowser/2.0'},
    localStorage: {getItem: () => v2}, setTimeout() {}, clearTimeout() {},
    document: {
      documentElement: node('html'), getElementById: node,
      querySelectorAll(selector) {
        return selector.startsWith('.provider-choice') ? [provider].filter(input => !selector.endsWith(':checked') || input.checked) : [];
      },
    },
    async fetch(url, options) {
      calls.push([options.method || 'GET', url]);
      assert.ok(options.headers.Authorization === 'Bearer ' + fixtureKey, 'expected the remembered key');
      if (url.endsWith('/state')) return json({settings, plan});
      if (url.endsWith('/settings')) {
        assert.equal(options.headers['Content-Type'], 'application/json');
        assert.deepEqual(JSON.parse(options.body).providers, ['codex']);
        return json({saved: true});
      }
      if (url.endsWith('/preview')) return previewResponse || json(plan);
      throw new Error('unexpected fixture request');
    },
  });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(node('connectionNotice').hidden, true);
  return {node, calls};
}

const managementPath = '/v0/management/plugins/credential-tier-router';

for (const id of ['previewButton', 'refreshButton']) {
  test(`${id} saves settings then sends POST /preview on the same origin`, async () => {
    const {node, calls} = await mountPlugin();
    await node(id).handlers.click();
    assert.deepEqual(calls, [
      ['GET', managementPath + '/state'],
      ['PUT', managementPath + '/settings'],
      ['POST', managementPath + '/preview'],
    ]);
    assert.equal(node('liveStatus').textContent, 'Preview complete; no credentials changed');
  });
}

test('preview conflict and gateway errors render only fixed messages', async () => {
  for (const [status, message] of [[409, 'A quota check is already in progress'], [502, 'Request failed: HTTP 502']]) {
    const {node} = await mountPlugin({ok: false, status, json: async () => ({error: fixtureKey})});
    await node('previewButton').handlers.click();
    assert.equal(node('liveStatus').textContent, message);
  }
});
