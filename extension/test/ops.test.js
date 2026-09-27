import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { validate, createRunner, errorFrame, grantedSites, helloMessage, geminiImageURL, geminiImageURLs, parseBatchexecute, OpError, OPS, SITE_ACCESS, CHUNK_BYTES, MAX_FILE_BYTES, MAX_MESSAGE_BYTES } from '../ops.js';

const fixture = (p) => JSON.parse(readFileSync(new URL('../../internal/history/testdata/' + p, import.meta.url)));

function jsonResponse(body, status = 200, type = 'application/json') {
  return new Response(typeof body === 'string' ? body : JSON.stringify(body), { status, headers: { 'content-type': type } });
}

function bytesResponse(bytes, type = 'image/png') {
  return new Response(bytes, { status: 200, headers: { 'content-type': type, 'content-length': String(bytes.length) } });
}

// fakeFetch routes by exact URL and records every call.
function fakeFetch(routes) {
  const calls = [];
  const fn = async (url, init = {}) => {
    calls.push({ url: String(url), init });
    const h = routes[String(url)];
    if (!h) return jsonResponse({ detail: 'not found' }, 404);
    return typeof h === 'function' ? h(url, init) : h.clone();
  };
  fn.calls = calls;
  return fn;
}

async function run(runner, op, args) {
  const frames = [];
  await runner.run(op, args, (f) => frames.push(f));
  return frames;
}

const SESSION = 'https://chatgpt.com/api/auth/session';
const TOKEN = 'secret-access-token-never-returned';

test('validate accepts only the fixed operation set with exact args', () => {
  assert.deepEqual([...OPS].sort(), ['chatgpt.close', 'chatgpt.detail', 'chatgpt.file', 'chatgpt.list', 'chatgpt.send', 'claudeai.close', 'claudeai.detail', 'claudeai.file', 'claudeai.list', 'claudeai.send', 'extension.reload', 'gemini.close', 'gemini.detail', 'gemini.file', 'gemini.list', 'gemini.send']);
  assert.deepEqual(validate({ id: 1, op: 'chatgpt.list', args: { count: 5 } }), { id: 1, op: 'chatgpt.list', args: { count: 5 } });
  validate({ id: 2, op: 'chatgpt.file', args: { file_id: 'file_00000000abcd1234', conversation_id: 'abc-1' } });
  validate({ id: 3, op: 'claudeai.detail', args: { id: 'c1a0d000-0000-4000-8000-000000000001' } });
  assert.deepEqual(validate({ id: 4, op: 'chatgpt.send', args: { message: 'hello' } }).args, { message: 'hello' });
  assert.deepEqual(validate({ id: 5, op: 'claudeai.send', args: { message: 'x'.repeat(MAX_MESSAGE_BYTES), conversation_id: 'abc-1', new_chat: false } }).args.conversation_id, 'abc-1');
  assert.deepEqual(validate({ id: 6, op: 'chatgpt.send', args: { message: 'hi', new_chat: true } }).args, { message: 'hi', new_chat: true });
  assert.deepEqual(validate({ id: 7, op: 'claudeai.close', args: { conversation_id: 'abc-1' } }).args, { conversation_id: 'abc-1' });
  assert.deepEqual(validate({ id: 7, op: 'extension.reload', args: {} }), { id: 7, op: 'extension.reload', args: {} });
  const bad = [
    null,
    'chatgpt.list',
    { id: 1, op: 'chatgpt.eval', args: { count: 1 } },
    { id: 1, op: 'constructor', args: {} },
    { id: -1, op: 'chatgpt.list', args: { count: 1 } },
    { id: 1.5, op: 'chatgpt.list', args: { count: 1 } },
    { id: 1, op: 'chatgpt.list', args: { count: 0 } },
    { id: 1, op: 'chatgpt.list', args: { count: 101 } },
    { id: 1, op: 'chatgpt.list', args: { count: 2.5 } },
    { id: 1, op: 'chatgpt.list', args: { count: '5' } },
    { id: 1, op: 'chatgpt.list', args: { count: 1, code: 'alert(1)' } },
    { id: 1, op: 'chatgpt.detail', args: { id: '../../backend-api/me' } },
    { id: 1, op: 'chatgpt.detail', args: { id: 'a.b' } },
    { id: 1, op: 'chatgpt.detail', args: { id: 'abc?x=1' } },
    { id: 1, op: 'chatgpt.detail', args: { id: 'x'.repeat(129) } },
    { id: 1, op: 'claudeai.file', args: { file_id: 'f1', conversation_id: 'c1' } },
    { id: 1, op: 'claudeai.detail', args: {} },
    { id: 1, op: 'chatgpt.list' },
    { id: 1, op: 'chatgpt.list', args: { count: 1 }, extra: true },
    { id: 1, op: 'chatgpt.send', args: {} },
    { id: 1, op: 'chatgpt.send', args: { message: '' } },
    { id: 1, op: 'chatgpt.send', args: { message: '  \n ' } },
    { id: 1, op: 'chatgpt.send', args: { message: 42 } },
    { id: 1, op: 'chatgpt.send', args: { message: 'x'.repeat(MAX_MESSAGE_BYTES + 1) } },
    { id: 1, op: 'chatgpt.send', args: { message: '\u00e9'.repeat(MAX_MESSAGE_BYTES / 2 + 1) } },
    { id: 1, op: 'chatgpt.send', args: { message: 'hi', conversation_id: '../c/x' } },
    { id: 1, op: 'chatgpt.send', args: { message: 'hi', new_chat: 'yes' } },
    { id: 1, op: 'chatgpt.send', args: { message: 'hi', new_chat: true, conversation_id: 'abc' } },
    { id: 1, op: 'claudeai.send', args: { message: 'hi', code: 'alert(1)' } },
    { id: 1, op: 'claudeai.send', args: { message: 'hi', url: 'https://evil.example/' } },
    { id: 1, op: 'chatgpt.close', args: {} },
    { id: 1, op: 'chatgpt.close', args: { conversation_id: '../c/x' } },
    { id: 1, op: 'claudeai.close', args: { conversation_id: 'abc', tab_id: 5 } },
    { id: 1, op: 'extension.reload', args: { now: true } },
    { id: 1, op: 'extension.reload' },
  ];
  for (const m of bad) {
    assert.throws(() => validate(m), (e) => e.code === 'bad_request', JSON.stringify(m));
  }
});

test('chatgpt.list gets the token in the worker and never returns it', async () => {
  const list = fixture('chatgpt/conversations.json');
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN, user: { id: 'u' } }),
    'https://chatgpt.com/backend-api/conversations?offset=0&limit=20&order=updated': jsonResponse(list),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.list', { count: 20 });
  assert.equal(frames.length, 1);
  assert.equal(frames[0].ok, true);
  assert.deepEqual(frames[0].result, list);
  assert.ok(!JSON.stringify(frames).includes(TOKEN));
  assert.equal(f.calls[0].init.credentials, 'include');
  assert.equal(f.calls[1].init.headers.Authorization, 'Bearer ' + TOKEN);
  assert.equal(f.calls[1].init.credentials, 'include');
  assert.equal(f.calls[1].init.method, 'GET');
});

test('chatgpt.detail fetches one conversation', async () => {
  const id = '6a1f0c2e-1111-4a2b-9c3d-000000000001';
  const detail = fixture(`chatgpt/conversation-${id}.json`);
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [`https://chatgpt.com/backend-api/conversation/${id}`]: jsonResponse(detail),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.detail', { id });
  assert.deepEqual(frames[0].result, detail);
});

test('chatgpt not logged in maps to not_logged_in', async () => {
  const f = fakeFetch({ [SESSION]: jsonResponse({}) });
  await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => e.code === 'not_logged_in');
  const f401 = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated': jsonResponse({ detail: 'expired' }, 401),
  });
  await assert.rejects(run(createRunner({ fetch: f401 }), 'chatgpt.list', { count: 1 }), (e) => e.code === 'not_logged_in' && !e.message.includes(TOKEN));
});

test('error classes: blocked, endpoint changed, not found, rate limited', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const cases = [
    [jsonResponse('<html>Just a moment...</html>', 403, 'text/html'), 'blocked'],
    [jsonResponse({}, 404), 'endpoint_changed'],
    [jsonResponse('<html>not json</html>', 200, 'text/html'), 'endpoint_changed'],
    [jsonResponse({}, 429), 'rate_limited'],
    [jsonResponse({}, 500), 'http_error'],
  ];
  for (const [resp, code] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: resp });
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => e.code === code, code);
  }
  const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }) });
  await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.detail', { id: 'missing-1' }), (e) => e.code === 'not_found');
});

test('rate_limited carries retry_after seconds from the Retry-After header', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const limited = (headers) => new Response('{}', { status: 429, headers: { 'content-type': 'application/json', ...headers } });
  const soon = new Date(Date.now() + 90_000).toUTCString();
  const cases = [
    [{ 'retry-after': '120' }, 120],
    [{ 'retry-after': soon }, [88, 91]],
    [{ 'retry-after': 'Wed, 21 Oct 2015 07:28:00 GMT' }, undefined],
    [{ 'retry-after': 'soon' }, undefined],
    [{ 'retry-after': '999999' }, 3600],
    [{}, undefined],
  ];
  for (const [headers, want] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: limited(headers) });
    let caught;
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => {
      caught = e;
      return e.code === 'rate_limited';
    });
    const frame = errorFrame(caught);
    assert.equal(frame.ok, false);
    assert.equal(frame.error.code, 'rate_limited');
    assert.match(frame.error.message, /HTTP 429 from \/backend-api\/conversations/);
    if (Array.isArray(want)) {
      assert.ok(frame.error.retry_after >= want[0] && frame.error.retry_after <= want[1], `${JSON.stringify(headers)}: ${frame.error.retry_after}`);
    } else if (want === undefined) {
      assert.ok(!('retry_after' in frame.error), `${JSON.stringify(headers)}: ${JSON.stringify(frame.error)}`);
    } else {
      assert.equal(frame.error.retry_after, want, JSON.stringify(headers));
    }
  }
  // Other errors carry no retry_after; unknown errors are internal.
  assert.deepEqual(errorFrame(new OpError('http_error', 'HTTP 502 from /x')), { ok: false, error: { code: 'http_error', message: 'HTTP 502 from /x' } });
  assert.deepEqual(errorFrame(new Error('secret detail')), { ok: false, error: { code: 'internal', message: 'internal error' } });
});

function pngBytes(n) {
  const b = new Uint8Array(n);
  b.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  for (let i = 8; i < n; i++) b[i] = i % 251;
  return b;
}

function reassemble(frames) {
  const parts = frames.map((f, i) => {
    assert.equal(f.ok, true);
    assert.equal(f.chunk.seq, i);
    assert.equal(f.chunk.last, i === frames.length - 1);
    return Buffer.from(f.chunk.data, 'base64');
  });
  return Buffer.concat(parts);
}

test('chatgpt.file (file-service) follows download_url on an allowed host, in bounded chunks', async () => {
  const img = pngBytes(CHUNK_BYTES * 2 + 17);
  const signed = 'https://files.oaiusercontent.com/file-Sk3tchAbc123?se=2026&sig=fake';
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    'https://chatgpt.com/backend-api/files/file-Sk3tchAbc123/download': jsonResponse({ status: 'success', download_url: signed, file_name: 'sketch.png' }),
    [signed]: bytesResponse(img),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.file', { file_id: 'file-Sk3tchAbc123' });
  assert.equal(frames.length, 3);
  for (const fr of frames) {
    assert.ok(fr.chunk.data.length <= Math.ceil(CHUNK_BYTES / 3) * 4);
    assert.equal(fr.mime, 'image/png');
    assert.equal(fr.size, img.length);
  }
  assert.deepEqual(new Uint8Array(reassemble(frames)), img);
  const signedCall = f.calls.find((c) => c.url === signed);
  assert.equal(signedCall.init.credentials, 'omit');
  assert.equal(signedCall.init.headers, undefined);
});

test('chatgpt.file (sediment) uses the newer download endpoint with the conversation id', async () => {
  const img = pngBytes(100);
  const u = 'https://chatgpt.com/backend-api/files/download/file_00000000abcd1234?conversation_id=conv-1&inline=false';
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [u]: jsonResponse({ status: 'success', download_url: '/backend-api/estuary/content?id=file_00000000abcd1234&sig=x' }),
    'https://chatgpt.com/backend-api/estuary/content?id=file_00000000abcd1234&sig=x': bytesResponse(img),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.file', { file_id: 'file_00000000abcd1234', conversation_id: 'conv-1' });
  assert.deepEqual(new Uint8Array(reassemble(frames)), img);
  const est = f.calls.at(-1);
  assert.equal(est.init.credentials, 'include');
});

test('chatgpt.file refuses download_url on a host outside the allowlist', async () => {
  for (const bad of ['https://evil.example/x.png', 'http://files.oaiusercontent.com/x', 'https://oaiusercontent.com.evil.example/x', 'javascript:alert(1)']) {
    const f = fakeFetch({
      [SESSION]: jsonResponse({ accessToken: TOKEN }),
      'https://chatgpt.com/backend-api/files/file-A1/download': jsonResponse({ status: 'success', download_url: bad }),
    });
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.file', { file_id: 'file-A1' }), (e) => e.code === 'endpoint_changed', bad);
    assert.equal(f.calls.length, 2, 'no fetch of ' + bad);
  }
});

test('file size cap and content type', async () => {
  const big = new Response(new Uint8Array(8), { status: 200, headers: { 'content-type': 'image/png', 'content-length': String(MAX_FILE_BYTES + 1) } });
  const org = fixture('claudeai/organizations.json');
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(org),
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f1/preview': big,
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f2/preview': bytesResponse(new Uint8Array(10), 'text/html'),
  });
  const r = createRunner({ fetch: f });
  await assert.rejects(run(r, 'claudeai.file', { file_id: 'f1' }), (e) => e.code === 'too_large');
  await assert.rejects(run(r, 'claudeai.file', { file_id: 'f2' }), (e) => e.code === 'endpoint_changed');
});

test('file size cap applies to a streamed body with no content-length', async () => {
  let pulled = 0;
  let cancelled = false;
  const chunk = new Uint8Array(1024 * 1024);
  const body = new ReadableStream({
    pull(c) {
      pulled += chunk.length;
      c.enqueue(chunk);
      if (pulled > MAX_FILE_BYTES * 4) c.close();
    },
    cancel() {
      cancelled = true;
    },
  });
  const res = new Response(body, { status: 200, headers: { 'content-type': 'image/png' } });
  assert.equal(res.headers.get('content-length'), null);
  const org = fixture('claudeai/organizations.json');
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(org),
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f3/preview': () => res,
  });
  const frames = [];
  await assert.rejects(
    createRunner({ fetch: f }).run('claudeai.file', { file_id: 'f3' }, (fr) => frames.push(fr)),
    (e) => e.code === 'too_large' && e.message === `file over ${MAX_FILE_BYTES} bytes`,
  );
  assert.equal(frames.length, 0);
  assert.ok(pulled <= MAX_FILE_BYTES + 2 * chunk.length, `read ${pulled} bytes before refusing`);
  assert.ok(cancelled, 'body stream cancelled');
});

test('a streamed body with no content-length under the cap is emitted whole', async () => {
  const img = pngBytes(CHUNK_BYTES + 5);
  const body = new ReadableStream({
    start(c) {
      c.enqueue(img.subarray(0, 100));
      c.enqueue(img.subarray(100));
      c.close();
    },
  });
  const org = fixture('claudeai/organizations.json');
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(org),
    'https://claude.ai/api/0rg00000-0000-4000-8000-000000000000/files/f4/preview': () => new Response(body, { status: 200, headers: { 'content-type': 'image/png' } }),
  });
  const frames = await run(createRunner({ fetch: f }), 'claudeai.file', { file_id: 'f4' });
  assert.equal(frames.length, 2);
  assert.equal(frames[0].size, img.length);
  assert.deepEqual(new Uint8Array(reassemble(frames)), img);
});

test('claudeai list, detail and file use the organization from /api/organizations', async () => {
  const org = '0rg00000-0000-4000-8000-000000000000';
  const list = fixture('claudeai/chat_conversations.json');
  const cid = 'c1a0d000-0000-4000-8000-000000000001';
  const detail = fixture(`claudeai/conversation-${cid}.json`);
  const img = pngBytes(500);
  const f = fakeFetch({
    'https://claude.ai/api/organizations': jsonResponse(fixture('claudeai/organizations.json')),
    [`https://claude.ai/api/organizations/${org}/chat_conversations?limit=2`]: jsonResponse(list),
    [`https://claude.ai/api/organizations/${org}/chat_conversations/${cid}?tree=True&rendering_mode=messages&render_all_tools=true`]: jsonResponse(detail),
    [`https://claude.ai/api/${org}/files/f11e0000-0000-4000-8000-0000000000aa/preview`]: bytesResponse(img, 'image/png'),
  });
  const r = createRunner({ fetch: f });
  const l = await run(r, 'claudeai.list', { count: 2 });
  assert.equal(l[0].result.length, 2, 'list trimmed to count');
  const d = await run(r, 'claudeai.detail', { id: cid });
  assert.deepEqual(d[0].result, detail);
  const fr = await run(r, 'claudeai.file', { file_id: 'f11e0000-0000-4000-8000-0000000000aa' });
  assert.deepEqual(new Uint8Array(reassemble(fr)), img);
  for (const c of f.calls) assert.equal(c.init.credentials, 'include');
  assert.equal(f.calls.filter((c) => c.url.endsWith('/api/organizations')).length, 1, 'org cached');
});

test('claudeai with no organization is not logged in', async () => {
  const f = fakeFetch({ 'https://claude.ai/api/organizations': jsonResponse([]) });
  await assert.rejects(run(createRunner({ fetch: f }), 'claudeai.list', { count: 1 }), (e) => e.code === 'not_logged_in');
  const f403 = fakeFetch({ 'https://claude.ai/api/organizations': jsonResponse({ error: { type: 'permission_error' } }, 403) });
  await assert.rejects(run(createRunner({ fetch: f403 }), 'claudeai.list', { count: 1 }), (e) => e.code === 'not_logged_in');
});

test('message content is data: a detail containing script text is returned untouched and never run', async () => {
  const id = 'evil-1';
  let ran = false;
  globalThis.__tincanPwned = () => { ran = true; };
  const detail = { title: 'x', mapping: { a: { message: { content: { parts: ['__tincanPwned()', '<script>__tincanPwned()</script>'] } } } } };
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [`https://chatgpt.com/backend-api/conversation/${id}`]: jsonResponse(detail),
  });
  const frames = await run(createRunner({ fetch: f }), 'chatgpt.detail', { id });
  assert.deepEqual(frames[0].result, detail);
  assert.equal(ran, false);
  delete globalThis.__tincanPwned;
});

test('worker code has no dynamic code execution', () => {
  const src = (f) => readFileSync(new URL(f, import.meta.url), 'utf8');
  for (const f of ['../ops.js', '../background.js', '../send.js', '../options.js']) {
    for (const banned of [/\beval\s*\(/, /new\s+Function\s*\(/, /importScripts\s*\(/, /set(Timeout|Interval)\s*\(\s*['"`]/, /chrome\.(debugger|webRequest|cookies|downloads)/]) {
      assert.ok(!banned.test(src(f)), `${f} matches ${banned}`);
    }
  }
  // ops.js stays fetch-only; tabs and scripting are reached only through
  // the sender background.js builds.
  const code = (f) => src(f).replace(/^\s*\/\/.*$/gm, '');
  assert.ok(!/chrome\./.test(code('../ops.js')), 'ops.js uses no chrome API');
  assert.ok(!/chrome\./.test(code('../send.js')), 'send.js gets tabs and scripting injected');
  const bg = src('../background.js');
  assert.deepEqual(bg.match(/chrome\.(tabs|scripting)\b/g), ['chrome.tabs', 'chrome.scripting']);
  // Every injection is a fixed function with its data as args, and only
  // send.js injects.
  assert.ok(!/executeScript/.test(bg) && !/executeScript/.test(src('../ops.js')));
  const send = src('../send.js');
  const calls = send.match(/executeScript\([^)]*\)/g);
  assert.deepEqual(calls, ['executeScript({ target: { tabId }, world: \'ISOLATED\', func, args })']);
  const injected = [...send.matchAll(/inject\((?:tab\.id|entry\[0\]), (\w+),/g)].map((m) => m[1]);
  assert.equal(injected.length, [...send.matchAll(/await inject\(/g)].length, 'every injection is listed');
  for (const f of injected) {
    assert.ok(['pageProbe', 'pageFill', 'pageSubmit', 'pageFetchImage'].includes(f), f);
  }
});

// ---- Site access: every operation but close needs its site's grant.

// fakePermissions answers chrome.permissions.contains from a set of
// granted origins.
function fakePermissions(granted) {
  const asked = [];
  return {
    asked,
    granted: new Set(granted),
    async contains({ origins }) {
      asked.push(origins);
      return origins.every((o) => this.granted.has(o));
    },
  };
}

const ALL_ORIGINS = Object.values(SITE_ACCESS).flatMap((s) => s.origins);

test('the manifest asks for exactly the origins in SITE_ACCESS', () => {
  const m = JSON.parse(readFileSync(new URL('../manifest.json', import.meta.url), 'utf8'));
  const required = Object.values(SITE_ACCESS).filter((s) => s.required).flatMap((s) => s.origins);
  const optional = Object.values(SITE_ACCESS).filter((s) => !s.required).flatMap((s) => s.origins);
  // ChatGPT and claude.ai stay required, so an upgrade asks for nothing new.
  assert.deepEqual(m.host_permissions, ['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*', 'https://claude.ai/*']);
  assert.deepEqual(m.host_permissions, required);
  assert.deepEqual(m.optional_host_permissions || [], optional);
  assert.equal(m.options_ui.page, 'options.html');
  for (const s of Object.values(SITE_ACCESS)) {
    assert.ok(typeof s.label === 'string' && s.label !== '');
    assert.ok(s.origins.length > 0 && s.origins.every((o) => /^https:\/\/[a-z0-9.*-]+\/\*$/.test(o)), s.label);
    assert.ok(s.pageOrigins.length > 0 && s.pageOrigins.every((o) => s.origins.includes(o)), s.label);
  }
  // Every op prefix but extension.* has a site entry.
  for (const op of OPS) {
    const prefix = op.split('.')[0];
    if (prefix !== 'extension') assert.ok(Object.hasOwn(SITE_ACCESS, prefix), op);
  }
});

test('an op for a site without its grant fails permission_missing and opens no tab or fetch', async () => {
  const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }) });
  const sent = [];
  const closed = [];
  const sender = {
    send: async (site, a) => (sent.push(site), { conversation_id: 'c1', url: '', submitted_at: 1 }),
    close: async (site, id) => (closed.push([site, id]), { closed: 1 }),
  };
  const perms = fakePermissions(['https://claude.ai/*']);
  const r = createRunner({ fetch: f, sender, permissions: perms });
  for (const [op, args] of [['chatgpt.send', { message: 'hi' }], ['chatgpt.list', { count: 1 }], ['chatgpt.detail', { id: 'c1' }], ['chatgpt.file', { file_id: 'file-1' }]]) {
    let caught;
    await assert.rejects(run(r, op, args), (e) => ((caught = e), e.code === 'permission_missing'), op);
    assert.match(errorFrame(caught).error.message, /options page/);
  }
  assert.equal(f.calls.length, 0, 'no fetch without the grant');
  assert.deepEqual(sent, [], 'no tab without the grant');
  // Closing a tab the extension opened needs no site access: a grant
  // revoked while a reply is read still lets its tab close.
  assert.deepEqual(await run(r, 'chatgpt.close', { conversation_id: 'c1' }), [{ ok: true, result: { closed: 1 } }]);
  assert.deepEqual(closed, [['chatgpt', 'c1']]);
  assert.deepEqual(perms.asked[0], [...SITE_ACCESS.chatgpt.pageOrigins]);

  // Granted: the op runs.
  perms.granted.add('https://chatgpt.com/*');
  perms.granted.add('https://*.oaiusercontent.com/*');
  const frames = await run(r, 'chatgpt.send', { message: 'hi' });
  assert.equal(frames[0].result.conversation_id, 'c1');
  assert.deepEqual(sent, ['chatgpt']);

  // A permissions API that throws counts as not granted.
  const broken = createRunner({ fetch: f, sender, permissions: { contains: async () => { throw new Error('x'); } } });
  await assert.rejects(run(broken, 'claudeai.list', { count: 1 }), (e) => e.code === 'permission_missing');
});

// ChatGPT's file host (*.oaiusercontent.com) is not needed to list or
// read conversations: withholding it leaves those ops working, the site
// counts as granted in the hello, and a file fetch from that host still
// fails as it would without the grant.
test('a site with only its page origin granted passes list and detail', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const detailURL = 'https://chatgpt.com/backend-api/conversation/c1';
  const f = fakeFetch({
    [SESSION]: jsonResponse({ accessToken: TOKEN }),
    [listURL]: jsonResponse({ items: [] }),
    [detailURL]: jsonResponse({ id: 'c1', mapping: {} }),
  });
  const perms = fakePermissions(['https://chatgpt.com/*']);
  const r = createRunner({ fetch: f, permissions: perms });
  await run(r, 'chatgpt.list', { count: 1 });
  await run(r, 'chatgpt.detail', { id: 'c1' });
  assert.ok(f.calls.some((c) => c.url === listURL), 'list fetched');
  assert.ok(f.calls.some((c) => c.url === detailURL), 'detail fetched');
  assert.deepEqual(await grantedSites(perms), ['chatgpt']);
  // The page origin alone withheld: permission_missing.
  perms.granted = new Set(['https://*.oaiusercontent.com/*']);
  await assert.rejects(run(r, 'chatgpt.list', { count: 1 }), (e) => e.code === 'permission_missing');
});

test('the hello lists the granted sites', async () => {
  const files = { 'manifest.json': 'x' };
  const perms = fakePermissions(['https://claude.ai/*']);
  const h = await helloMessage({ manifest: { version: '1.0.0' }, files, permissions: perms });
  assert.deepEqual(h, { id: 0, hello: { version: '1.0.0', unpacked: true, files, granted: ['claudeai'] } });
  perms.granted = new Set(ALL_ORIGINS);
  assert.deepEqual((await grantedSites(perms)).sort(), Object.keys(SITE_ACCESS).sort());
});

// The anti-bot sniff reads at most 64 KiB of a body, not all of it.
test('a large non-JSON answer is read only up to the sniff cap', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  let pulled = 0;
  const big = () => new Response(new ReadableStream({
    pull(c) {
      if (pulled >= 8 * 1024 * 1024) return c.close();
      pulled += 16 * 1024;
      c.enqueue(new Uint8Array(16 * 1024).fill(0x61));
    },
  }, { highWaterMark: 0 }), { status: 200, headers: { 'content-type': 'text/html' } });
  const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: big });
  const r = createRunner({ fetch: f });
  await assert.rejects(run(r, 'chatgpt.list', { count: 1 }), (e) => e.code === 'endpoint_changed');
  assert.ok(pulled <= 256 * 1024, `read ${pulled} bytes`);
});

test('anti-bot pages are blocked; a plain 401 or a 403 permission error is not_logged_in', async () => {
  const listURL = 'https://chatgpt.com/backend-api/conversations?offset=0&limit=1&order=updated';
  const redirected = (res, url) => {
    Object.defineProperty(res, 'url', { value: url });
    Object.defineProperty(res, 'redirected', { value: true });
    return res;
  };
  const cases = [
    ['cloudflare header on a 403', () => new Response('{}', { status: 403, headers: { 'content-type': 'application/json', 'cf-mitigated': 'challenge' } }), 'blocked'],
    ['cloudflare header on a 503', () => new Response('<html></html>', { status: 503, headers: { 'content-type': 'text/html', 'cf-mitigated': 'challenge' } }), 'blocked'],
    ['challenge page served 200', () => jsonResponse('<!DOCTYPE html><title>Just a moment...</title>', 200, 'text/html'), 'blocked'],
    ['403 JSON with an anti-bot marker', () => jsonResponse({ error: { code: 7, message: 'Request rejected by anti-bot rules.' } }, 403), 'blocked'],
    ['google /sorry/ interstitial', () => redirected(jsonResponse('<html>unusual traffic</html>', 429, 'text/html'), 'https://www.google.com/sorry/index?continue=x'), 'blocked'],
    ['google /sorry/ served 200', () => redirected(jsonResponse('<html></html>', 200, 'text/html'), 'https://www.google.com/sorry/index'), 'blocked'],
    ['plain 401', () => jsonResponse({ detail: 'expired' }, 401), 'not_logged_in'],
    ['401 HTML', () => jsonResponse('<html>Just a moment...</html>', 401, 'text/html'), 'not_logged_in'],
    ['403 permission error', () => jsonResponse({ error: { type: 'permission_error' } }, 403), 'not_logged_in'],
  ];
  for (const [name, resp, code] of cases) {
    const f = fakeFetch({ [SESSION]: jsonResponse({ accessToken: TOKEN }), [listURL]: () => resp() });
    await assert.rejects(run(createRunner({ fetch: f }), 'chatgpt.list', { count: 1 }), (e) => e.code === code, name);
  }
});

test('a session probe redirected to another host is not_logged_in and nothing is sent', async () => {
  const sent = [];
  const sender = { send: async (site) => (sent.push(site), { conversation_id: 'c1' }) };
  const moved = (url) => () => {
    const res = jsonResponse('<html>Log in</html>', 200, 'text/html');
    Object.defineProperty(res, 'url', { value: url });
    Object.defineProperty(res, 'redirected', { value: true });
    return res;
  };
  const chat = createRunner({ fetch: fakeFetch({ [SESSION]: moved('https://auth.openai.com/log-in') }), sender });
  await assert.rejects(run(chat, 'chatgpt.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  const claude = createRunner({ fetch: fakeFetch({ 'https://claude.ai/api/organizations': moved('https://accounts.google.com/v3/signin/identifier') }), sender });
  await assert.rejects(run(claude, 'claudeai.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  assert.deepEqual(sent, []);
  // A same-host redirect is followed as before.
  const same = fakeFetch({
    [SESSION]: () => {
      const res = jsonResponse({ accessToken: TOKEN });
      Object.defineProperty(res, 'url', { value: 'https://chatgpt.com/api/auth/session?x=1' });
      Object.defineProperty(res, 'redirected', { value: true });
      return res;
    },
  });
  await run(createRunner({ fetch: same, sender }), 'chatgpt.send', { message: 'hi' });
  assert.deepEqual(sent, ['chatgpt']);
});

// ---- Gemini: the app page's session values, then batchexecute.

const GEMINI_APP = 'https://gemini.google.com/app';
const GEMINI_RPC = 'https://gemini.google.com/_/BardChatUi/data/batchexecute';
const APP_HTML = '<html><script>window.WIZ_global_data = {"SNlM0e":"dummy-at-token","cfb2h":"boq_dummy_20260927.00_p0","FdrFJe":"-1234567890"};</script></html>';
const GEMINI_GRANT = ['https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*'];

// batchResponse renders inner as batchexecute's answer for rpcid: the
// guard, then length-prefixed chunks, the wrb.fr one first.
function batchResponse(rpcid, inner, status = 200) {
  const chunk = (v) => {
    const line = JSON.stringify(v);
    return `${line.length}\n${line}\n`;
  };
  const payload = inner === null ? null : JSON.stringify(inner);
  const text = ")]}'\n\n" + chunk([['wrb.fr', rpcid, payload, null, null, inner === null ? [3] : null, 'generic'], ['di', 42], ['af.httprm', 41, '-1', 7]]) + chunk([['e', 4, null, null, 120]]);
  return new Response(text, { status, headers: { 'content-type': 'application/json; charset=utf-8' } });
}

// geminiFetch answers the app page and batchexecute: rpc(rpcid, payload,
// call) returns the Response for each batchexecute call.
function geminiFetch({ app = () => new Response(APP_HTML, { status: 200, headers: { 'content-type': 'text/html' } }), rpc, other = {} }) {
  const calls = [];
  const fn = async (url, init = {}) => {
    const u = new URL(String(url));
    calls.push({ url: String(url), init });
    if (u.origin + u.pathname === GEMINI_APP) return app();
    if (u.origin + u.pathname === GEMINI_RPC) {
      const form = new URLSearchParams(init.body);
      const req = JSON.parse(form.get('f.req'));
      const [rpcid, payload] = req[0][0];
      return rpc(rpcid, JSON.parse(payload), { url: u, form });
    }
    const h = other[String(url)];
    if (h) return h();
    return jsonResponse({}, 404);
  };
  fn.calls = calls;
  return fn;
}

function redirectedTo(res, url) {
  Object.defineProperty(res, 'url', { value: url });
  Object.defineProperty(res, 'redirected', { value: true });
  return res;
}

test('gemini.list reads MaZiqc page by page with the app page session, which never leaves the worker', async () => {
  const pages = fixture('gemini/list.json').pages;
  const payloads = [];
  const f = geminiFetch({
    rpc: (rpcid, payload, { url, form }) => {
      assert.equal(rpcid, 'MaZiqc');
      assert.equal(form.get('at'), 'dummy-at-token');
      assert.equal(url.searchParams.get('bl'), 'boq_dummy_20260927.00_p0');
      assert.equal(url.searchParams.get('f.sid'), '-1234567890');
      assert.equal(url.searchParams.get('rpcids'), 'MaZiqc');
      assert.equal(url.searchParams.get('source-path'), '/app');
      assert.equal(url.searchParams.get('rt'), 'c');
      payloads.push(payload);
      return batchResponse('MaZiqc', payload[1] === null ? pages[0] : pages[1]);
    },
  });
  const r = createRunner({ fetch: f });
  const frames = await run(r, 'gemini.list', { count: 10 });
  // Exactly the list fixture the Go reader parses.
  assert.deepEqual(frames, [{ ok: true, result: { pages } }]);
  assert.deepEqual(payloads, [[13, null, [0, null, 1]], [13, 'dummy-page-token-2', [0, null, 1]]]);
  assert.ok(!JSON.stringify(frames).includes('dummy-at-token'));
  const rpcCalls = f.calls.filter((c) => c.url.startsWith(GEMINI_RPC));
  for (const c of rpcCalls) {
    assert.equal(c.init.method, 'POST');
    assert.equal(c.init.credentials, 'include');
    assert.match(c.init.headers['content-type'], /^application\/x-www-form-urlencoded/);
  }
  assert.notEqual(new URL(rpcCalls[0].url).searchParams.get('_reqid'), new URL(rpcCalls[1].url).searchParams.get('_reqid'));
  // A small count stops after the first page; the session is reused.
  const again = await run(r, 'gemini.list', { count: 2 });
  assert.equal(again[0].result.pages.length, 1);
  assert.equal(f.calls.filter((c) => c.url === GEMINI_APP).length, 1, 'the app page is fetched once');
});

test('gemini.detail reads hNvQHb with the c_ id; a missing conversation is not_found', async () => {
  const inner = fixture('gemini/conversation-00000000000000a1.json');
  const f = geminiFetch({
    rpc: (rpcid, payload) => {
      assert.equal(rpcid, 'hNvQHb');
      if (payload[0] === 'c_00000000000000a1') return batchResponse('hNvQHb', inner);
      return batchResponse('hNvQHb', null);
    },
  });
  const r = createRunner({ fetch: f });
  assert.deepEqual(await run(r, 'gemini.detail', { id: '00000000000000a1' }), [{ ok: true, result: inner }]);
  const sent = JSON.parse(JSON.parse(new URLSearchParams(f.calls.at(-1).init.body).get('f.req'))[0][0][1]);
  assert.deepEqual(sent, ['c_00000000000000a1', 10, null, 1, [0], [4], null, 1]);
  await assert.rejects(run(r, 'gemini.detail', { id: '00000000000000ff' }), (e) => e.code === 'not_found');
  await assert.rejects(run(r, 'gemini.detail', { id: 'c_00000000000000a1' }), (e) => e.code === 'bad_request');
  await assert.rejects(run(r, 'gemini.detail', { id: 'abc-1' }), (e) => e.code === 'bad_request');
});

test('parseBatchexecute: anything but a wrb.fr answer for the rpcid is endpoint_changed', () => {
  const ok = ")]}'\n\n40\n" + JSON.stringify([['wrb.fr', 'X', '[1,2]', null]]) + '\n';
  assert.deepEqual(parseBatchexecute(ok, 'X'), [1, 2]);
  assert.equal(parseBatchexecute(JSON.stringify([['wrb.fr', 'X', null, null, null, [5]]]), 'X'), null);
  for (const text of ['', ")]}'\n", '<html>nope</html>', JSON.stringify([['wrb.fr', 'Y', '[1]']]), JSON.stringify([['wrb.fr', 'X', '{bad json']]), JSON.stringify([['wrb.fr', 'X', 5]]), undefined]) {
    assert.throws(() => parseBatchexecute(text, 'X'), (e) => e.code === 'endpoint_changed', String(text));
  }
});

test('gemini: /sorry/ is blocked, a sign-in redirect or no session is not_logged_in, a stale token is refetched once', async () => {
  const html = (body = APP_HTML) => new Response(body, { status: 200, headers: { 'content-type': 'text/html' } });
  const cases = [
    ['app page on /sorry/', { app: () => redirectedTo(html('<html>unusual traffic</html>'), 'https://www.google.com/sorry/index?continue=x') }, 'blocked'],
    ['batchexecute on /sorry/', { rpc: () => redirectedTo(new Response('<html></html>', { status: 429, headers: { 'content-type': 'text/html' } }), 'https://www.google.com/sorry/index') }, 'blocked'],
    ['sign-in redirect', { app: () => redirectedTo(html('<html>Sign in</html>'), 'https://accounts.google.com/v3/signin/identifier') }, 'not_logged_in'],
    ['no session on the page', { app: () => html('<html>{"cfb2h":"boq_x"}</html>') }, 'not_logged_in'],
    ['no build label', { app: () => html('<html>{"SNlM0e":"tok"}</html>') }, 'endpoint_changed'],
    ['rate limited', { rpc: () => new Response('', { status: 429, headers: { 'retry-after': '30' } }) }, 'rate_limited'],
    ['odd payload', { rpc: () => new Response(")]}'\n\n5\n[[1]]\n", { status: 200, headers: { 'content-type': 'application/json' } }) }, 'endpoint_changed'],
  ];
  for (const [name, routes, code] of cases) {
    const f = geminiFetch({ rpc: () => batchResponse('MaZiqc', [null, null, []]), ...routes });
    await assert.rejects(run(createRunner({ fetch: f }), 'gemini.list', { count: 1 }), (e) => e.code === code, name);
  }
  let n = 0;
  const f = geminiFetch({ rpc: () => (++n === 1 ? new Response('', { status: 400 }) : batchResponse('MaZiqc', [null, null, []])) });
  assert.deepEqual(await run(createRunner({ fetch: f }), 'gemini.list', { count: 1 }), [{ ok: true, result: { pages: [[null, null, []]] } }]);
  assert.equal(f.calls.filter((c) => c.url === GEMINI_APP).length, 2, 'the session is fetched again after a 400');
});

test('geminiImageURL finds the n-th image of a response on the image host only', () => {
  const inner = fixture('gemini/conversation-00000000000000a1.json');
  assert.equal(geminiImageURL(inner, 'rc_00000000000000b2', 0), 'https://lh3.googleusercontent.com/gg/dummy-star-1');
  assert.equal(geminiImageURL(inner, 'rc_00000000000000b2', 1), null);
  assert.equal(geminiImageURL(inner, 'rc_00000000000000b1', 0), null);
  assert.equal(geminiImageURL(inner, 'rc_missing', 0), null);
  const cand = ['rc_9', ['text https://lh3.googleusercontent.com/in-text'], ['https://evil.example/x.png', 'https://lh3.googleusercontent.com/a', ['https://lh3.googleusercontent.com/b', 'https://lh3.googleusercontent.com/a'], 'https://lh3.googleusercontent.com:8443/c']];
  const odd = [[[['c_1', 'r_1'], null, [['hi']], [[cand]]]]];
  assert.deepEqual(geminiImageURLs(cand), ['https://lh3.googleusercontent.com/a', 'https://lh3.googleusercontent.com/b']);
  assert.equal(geminiImageURL(odd, 'rc_9', 1), 'https://lh3.googleusercontent.com/b');
  assert.equal(geminiImageURL('x', 'rc_9', 0), null);
});

test('gemini.file captures the image in the send tab first, then fetches it in the worker, and never takes a URL', async () => {
  const inner = fixture('gemini/conversation-00000000000000a1.json');
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1, 2, 3, 4]);
  const IMG = 'https://lh3.googleusercontent.com/gg/dummy-star-1';
  const args = { file_id: 'rc_00000000000000b2-0', conversation_id: '00000000000000a1' };
  const mk = (capture, image) => {
    const captured = [];
    const sender = { capture: async (site, conv, url, max) => (captured.push([site, conv, url, max]), capture()) };
    const f = geminiFetch({ rpc: () => batchResponse('hNvQHb', inner), other: { [IMG]: image } });
    return { r: createRunner({ fetch: f, sender }), f, captured };
  };
  // 1. Captured in the page.
  let t = mk(() => ({ ok: true, mime: 'image/png', data: Buffer.from(png).toString('base64') }), () => bytesResponse(png));
  let frames = await run(t.r, 'gemini.file', args);
  assert.deepEqual(t.captured, [['gemini', '00000000000000a1', IMG, MAX_FILE_BYTES]]);
  assert.equal(frames.length, 1);
  assert.deepEqual(Buffer.from(frames[0].chunk.data, 'base64'), Buffer.from(png));
  assert.equal(frames[0].mime, 'image/png');
  assert.ok(!t.f.calls.some((c) => c.url === IMG), 'no worker fetch once captured');
  // 2. The page could not: the worker fetches it with the image host's grant.
  t = mk(() => null, () => bytesResponse(png));
  frames = await run(t.r, 'gemini.file', args);
  assert.deepEqual(Buffer.from(frames[0].chunk.data, 'base64'), Buffer.from(png));
  assert.equal(t.f.calls.find((c) => c.url === IMG).init.credentials, 'include');
  // A capture answer that is not an image is not trusted.
  t = mk(() => ({ ok: true, mime: 'text/html', data: 'PGh0bWw+' }), () => bytesResponse(png));
  await run(t.r, 'gemini.file', args);
  assert.ok(t.f.calls.some((c) => c.url === IMG));
  // 3. Neither: the op fails, and the Go side notes the lost image.
  t = mk(() => null, () => new Response('', { status: 403, headers: { 'content-type': 'text/plain' } }));
  await assert.rejects(run(t.r, 'gemini.file', args), (e) => e instanceof OpError);
  // An image that is not in the conversation is not_found, with no capture.
  t = mk(() => null, () => bytesResponse(png));
  await assert.rejects(run(t.r, 'gemini.file', { ...args, file_id: 'rc_00000000000000b2-3' }), (e) => e.code === 'not_found');
  assert.deepEqual(t.captured, []);
  assert.throws(() => validate({ id: 1, op: 'gemini.file', args: { file_id: 'rc_1-0' } }), (e) => e.code === 'bad_request', 'conversation id required');
  assert.throws(() => validate({ id: 1, op: 'gemini.file', args: { file_id: 'https://lh3.googleusercontent.com/x', conversation_id: '00000000000000a1' } }), (e) => e.code === 'bad_request');
  await assert.rejects(run(t.r, 'gemini.file', { ...args, file_id: 'rc_1' }), (e) => e.code === 'bad_request');
});

test('gemini.send checks the session fresh first; logged out or blocked opens no tab', async () => {
  const sent = [];
  const sender = { send: async (site, a) => (sent.push([site, a]), { conversation_id: '00000000000000d1', url: '', submitted_at: 1 }) };
  const ok = geminiFetch({ rpc: () => batchResponse('MaZiqc', [null, null, []]) });
  const r = createRunner({ fetch: ok, sender });
  await run(r, 'gemini.list', { count: 1 });
  const frames = await run(r, 'gemini.send', { message: 'hi', conversation_id: '00000000000000d1' });
  assert.equal(frames[0].result.conversation_id, '00000000000000d1');
  assert.deepEqual(sent, [['gemini', { message: 'hi', conversation_id: '00000000000000d1' }]]);
  assert.equal(ok.calls.filter((c) => c.url === GEMINI_APP).length, 2, 'the send asked the app page again');
  await assert.rejects(run(r, 'gemini.send', { message: 'hi', conversation_id: 'abc-1' }), (e) => e.code === 'bad_request');
  const out = geminiFetch({ app: () => redirectedTo(new Response('<html></html>', { status: 200, headers: { 'content-type': 'text/html' } }), 'https://accounts.google.com/ServiceLogin') });
  await assert.rejects(run(createRunner({ fetch: out, sender }), 'gemini.send', { message: 'hi' }), (e) => e.code === 'not_logged_in');
  const sorry = geminiFetch({ app: () => redirectedTo(new Response('', { status: 429, headers: { 'content-type': 'text/html' } }), 'https://www.google.com/sorry/index') });
  await assert.rejects(run(createRunner({ fetch: sorry, sender }), 'gemini.send', { message: 'hi' }), (e) => e.code === 'blocked');
  assert.equal(sent.length, 1);
});

test('gemini ops need the Gemini page grant; the options page grants the image host with it', async () => {
  const f = geminiFetch({ rpc: () => batchResponse('MaZiqc', [null, null, []]) });
  const perms = fakePermissions(['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*', 'https://claude.ai/*', 'https://lh3.googleusercontent.com/*']);
  const r = createRunner({ fetch: f, permissions: perms });
  await assert.rejects(run(r, 'gemini.list', { count: 1 }), (e) => e.code === 'permission_missing' && /Gemini/.test(e.message));
  assert.equal(f.calls.length, 0);
  perms.granted.delete('https://lh3.googleusercontent.com/*');
  perms.granted.add('https://gemini.google.com/*');
  await run(r, 'gemini.list', { count: 1 });
  assert.deepEqual(SITE_ACCESS.gemini.origins, GEMINI_GRANT);
  assert.deepEqual(SITE_ACCESS.gemini.pageOrigins, ['https://gemini.google.com/*']);
  assert.equal(SITE_ACCESS.gemini.required, false, 'an optional site: upgrading asks for nothing');
});
