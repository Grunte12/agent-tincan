// Fixed operations for the Agent Tincan history bridge and web agents.
//
// The native host sends {id, op, args}. Only the operations in SPEC exist,
// and their arguments are only validated ids, integer counts, booleans and
// one capped message string. The read operations run the fixed fetch code
// below with the user's own session (credentials: 'include'). The send
// operations hand the message, as data, to the sender (send.js), which
// types it into a background tab the extension opens itself; the close
// operations close that tab once the reply is finished.
// extension.reload asks the worker to reload itself so Chrome re-reads the
// unpacked files after an update. Nothing in a message or a response is
// ever executed; responses are returned as data.
//
// Every operation but close first checks that Chrome has granted the
// extension its site's page origins (SITE_ACCESS) and fails permission_missing
// without a tab or a fetch when it has not. The hello lists the granted
// sites, and the worker says hello again whenever a grant changes.
//
// ChatGPT's access token is read from /api/auth/session inside this worker
// and used only for the Authorization header of the next requests. It is
// never returned, logged or stored.
//
// Gemini has no JSON API of its own: the worker fetches the app page
// (/app) for its per-session values (SNlM0e, cfb2h, FdrFJe), keeps them in
// memory for a few minutes, and calls the app's batchexecute endpoint with
// two fixed rpcids, MaZiqc (list, a page at a time) and hNvQHb (read one
// conversation). The decoded inner payloads go back as data; the Go side
// reads their positions. Gemini conversation ids travel as the URL's hex;
// the "c_" batchexecute wants is added here only. Images are fetched by
// gemini.file, which never takes a URL: it reads the conversation again
// and picks the image by response and position (see geminiImageURL), then
// asks the sender to capture it in the tab its send left open, and falls
// back to fetching it here.

export const NATIVE_HOST = 'com.agenttincan.history';
export const MAX_COUNT = 100;
export const MAX_FILE_BYTES = 10 * 1024 * 1024;
// A multiple of 3 so every chunk is whole base64; 512 KiB encoded per
// message.
export const CHUNK_BYTES = 384 * 1024;
// MAX_MESSAGE_BYTES caps a send op's message (UTF-8 bytes).
export const MAX_MESSAGE_BYTES = 32 * 1024;
// EXTENSION_FILES are the files the hello message reports hashes of, so the
// native host can tell when the unpacked files on disk have changed.
// Gemini: list page size, the most pages one list reads, and how long
// the app page's session values are reused before it is fetched again.
export const GEMINI_PAGE_SIZE = 13;
export const GEMINI_MAX_PAGES = 10;
export const GEMINI_SESSION_MS = 10 * 60 * 1000;
// GEMINI_IMAGE_PREFIX is where Gemini's images are served from; an image
// URL anywhere else is never fetched.
export const GEMINI_IMAGE_PREFIX = 'https://lh3.googleusercontent.com/';
export const EXTENSION_FILES = Object.freeze(['manifest.json', 'background.js', 'ops.js', 'send.js', 'options.html', 'options.js', 'icon16.png', 'icon48.png', 'icon128.png']);

// SITE_ACCESS is each site's host access, keyed by its op prefix: origins
// are all the origins its operations fetch and open tabs on (what the
// options page asks Chrome for), and pageOrigins the site's own pages,
// which must be granted before any of its operations runs. An origin in
// origins but not pageOrigins (ChatGPT's file host) is needed only by the
// fetches that reach it, which fail without it as any fetch to an
// ungranted host does, so withholding it leaves list and read working.
// A required site's origins
// are the manifest's host_permissions, granted at install (the owner can
// still withhold them in Chrome's site access settings); any other site's
// are optional_host_permissions, granted from the options page, so adding
// a site never disables an existing install until the owner accepts it.
export const SITE_ACCESS = Object.freeze({
  chatgpt: Object.freeze({ label: 'ChatGPT', origins: Object.freeze(['https://chatgpt.com/*', 'https://*.oaiusercontent.com/*']), pageOrigins: Object.freeze(['https://chatgpt.com/*']), required: true }),
  claudeai: Object.freeze({ label: 'claude.ai', origins: Object.freeze(['https://claude.ai/*']), pageOrigins: Object.freeze(['https://claude.ai/*']), required: true }),
  gemini: Object.freeze({ label: 'Gemini', origins: Object.freeze(['https://gemini.google.com/*', 'https://lh3.googleusercontent.com/*']), pageOrigins: Object.freeze(['https://gemini.google.com/*']), required: false }),
});

// siteGranted reports whether permissions (chrome.permissions) holds
// site's page origins, what its operations need, or with all set every
// one of its origins (the options page's full grant); an API failure
// counts as not granted.
export async function siteGranted(permissions, site, { all = false } = {}) {
  const s = SITE_ACCESS[site];
  if (!s) return false;
  try {
    return (await permissions.contains({ origins: [...(all ? s.origins : s.pageOrigins)] })) === true;
  } catch {
    return false;
  }
}

// grantedSites lists the SITE_ACCESS keys whose page origins are granted.
export async function grantedSites(permissions) {
  const out = [];
  for (const site of Object.keys(SITE_ACCESS)) {
    if (await siteGranted(permissions, site)) out.push(site);
  }
  return out;
}

// extension.reload waits while the sender has tabs (a reload would lose
// track of them), checking every RELOAD_RETRY_MS for at most
// RELOAD_MAX_WAIT_MS. At the cap it closes the finished tabs and reloads
// anyway; a send still typing at that point is abandoned (its tab stays
// open and the Go side's wait for it times out).
export const RELOAD_RETRY_MS = 5000;
export const RELOAD_MAX_WAIT_MS = 5 * 60 * 1000;

const ID_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const CHATGPT = 'https://chatgpt.com';
const CLAUDE = 'https://claude.ai';
const GEMINI = 'https://gemini.google.com';
// GEMINI_ID_RE is a Gemini conversation id as its URL shows it.
const GEMINI_ID_RE = /^[0-9a-f]{8,64}$/;

// Argument kinds: 'count' is a required integer 1..MAX_COUNT, 'id' a
// required id, 'id?' an optional id, 'bool?' an optional boolean and
// 'message' a required non-blank string of at most MAX_MESSAGE_BYTES.
const SEND_SPEC = Object.freeze({ message: 'message', conversation_id: 'id?', new_chat: 'bool?' });
const CLOSE_SPEC = Object.freeze({ conversation_id: 'id' });
const SPEC = Object.freeze({
  'chatgpt.list': Object.freeze({ count: 'count' }),
  'chatgpt.detail': Object.freeze({ id: 'id' }),
  'chatgpt.file': Object.freeze({ file_id: 'id', conversation_id: 'id?' }),
  'claudeai.list': Object.freeze({ count: 'count' }),
  'claudeai.detail': Object.freeze({ id: 'id' }),
  'claudeai.file': Object.freeze({ file_id: 'id' }),
  'chatgpt.send': SEND_SPEC,
  'claudeai.send': SEND_SPEC,
  'chatgpt.close': CLOSE_SPEC,
  'claudeai.close': CLOSE_SPEC,
  'gemini.list': Object.freeze({ count: 'count' }),
  'gemini.detail': Object.freeze({ id: 'id' }),
  'gemini.file': Object.freeze({ file_id: 'id', conversation_id: 'id' }),
  'gemini.send': SEND_SPEC,
  'gemini.close': CLOSE_SPEC,
  'extension.reload': Object.freeze({}),
});

export const OPS = new Set(Object.keys(SPEC));

// MAX_RETRY_AFTER_S caps a Retry-After reported to the host, in seconds.
export const MAX_RETRY_AFTER_S = 3600;

export class OpError extends Error {
  // retryAfter, for rate_limited, is the site's Retry-After in whole
  // seconds when it sent one.
  constructor(code, message, retryAfter) {
    super(message);
    this.code = code;
    if (Number.isSafeInteger(retryAfter) && retryAfter >= 0) this.retryAfter = retryAfter;
  }
}

// errorFrame is the failure frame for an error thrown by an operation:
// its code and message (retry_after too for a rate limit), or a bare
// internal error for anything that is not an OpError, so no unexpected
// detail leaves the extension.
export function errorFrame(e) {
  if (!(e instanceof OpError)) return { ok: false, error: { code: 'internal', message: 'internal error' } };
  const error = { code: e.code, message: e.message };
  if (e.retryAfter !== undefined) error.retry_after = e.retryAfter;
  return { ok: false, error };
}

// retryAfterSeconds reads a Retry-After header: delay seconds or an
// HTTP date. It returns whole seconds (capped at MAX_RETRY_AFTER_S), or
// undefined when the header is missing, malformed or in the past.
function retryAfterSeconds(res) {
  const v = (res.headers.get('retry-after') || '').trim();
  if (v === '') return undefined;
  let s;
  if (/^\d+$/.test(v)) {
    s = Number(v);
  } else {
    const at = Date.parse(v);
    if (Number.isNaN(at)) return undefined;
    s = Math.ceil((at - Date.now()) / 1000);
    if (s < 0) return undefined;
  }
  return Math.min(s, MAX_RETRY_AFTER_S);
}

const bad = (m) => new OpError('bad_request', m);

function isPlainObject(v) {
  return v !== null && typeof v === 'object' && !Array.isArray(v) && Object.getPrototypeOf(v) === Object.prototype;
}

// validate returns {id, op, args} for a well-formed request or throws an
// OpError with code bad_request.
export function validate(msg) {
  if (!isPlainObject(msg)) throw bad('request is not an object');
  for (const k of Object.keys(msg)) {
    if (k !== 'id' && k !== 'op' && k !== 'args') throw bad('unexpected field');
  }
  const { id, op, args } = msg;
  if (!Number.isSafeInteger(id) || id < 0) throw bad('invalid request id');
  if (typeof op !== 'string' || !Object.hasOwn(SPEC, op)) throw bad('unknown operation');
  if (!isPlainObject(args)) throw bad('missing args');
  const spec = SPEC[op];
  const out = {};
  for (const k of Object.keys(args)) {
    if (!Object.hasOwn(spec, k)) throw bad('unexpected argument');
  }
  for (const [k, kind] of Object.entries(spec)) {
    const v = args[k];
    if (kind === 'count') {
      if (!Number.isInteger(v) || v < 1 || v > MAX_COUNT) throw bad(`${k} must be an integer from 1 to ${MAX_COUNT}`);
      out[k] = v;
    } else if (v === undefined && (kind === 'id?' || kind === 'bool?')) {
      continue;
    } else if (kind === 'bool?') {
      if (typeof v !== 'boolean') throw bad(`${k} must be a boolean`);
      out[k] = v;
    } else if (kind === 'message') {
      if (typeof v !== 'string' || v.trim() === '') throw bad(`${k} must be a non-empty string`);
      if (new TextEncoder().encode(v).length > MAX_MESSAGE_BYTES) throw bad(`${k} is over ${MAX_MESSAGE_BYTES} bytes`);
      out[k] = v;
    } else {
      if (typeof v !== 'string' || !ID_RE.test(v)) throw bad(`invalid ${k}`);
      out[k] = v;
    }
  }
  if (out.new_chat === true && out.conversation_id !== undefined) throw bad('new_chat and conversation_id together');
  return { id, op, args: out };
}

async function sha256Hex(bytes) {
  const d = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
  return Array.from(d, (b) => b.toString(16).padStart(2, '0')).join('');
}

// hashFiles returns the sha256 of each of EXTENSION_FILES, read through
// fetch; a file that cannot be read is reported as ''. The worker calls it
// once when it starts, so the hashes describe the code Chrome loaded even
// after the unpacked files on disk change.
export async function hashFiles({ getURL, fetch }) {
  const files = {};
  for (const name of EXTENSION_FILES) {
    try {
      const res = await fetch(getURL(name), { cache: 'no-store' });
      files[name] = res.ok ? await sha256Hex(new Uint8Array(await res.arrayBuffer())) : '';
    } catch {
      files[name] = '';
    }
  }
  return files;
}

// helloMessage is what the worker tells the native host when it connects
// and when a site grant changes: its version, whether it is unpacked (only
// an unpacked extension picks up new files on reload), the sha256 of each
// of its files as Chrome loaded them (files when given, hashFiles from
// worker start, else hashed now), and, given permissions, the sites whose
// access Chrome has granted.
export async function helloMessage({ manifest, getURL, fetch, files, permissions }) {
  const hashes = files && typeof files === 'object' ? files : await hashFiles({ getURL, fetch });
  const m = manifest && typeof manifest === 'object' ? manifest : {};
  const hello = { version: typeof m.version === 'string' ? m.version : '', unpacked: !('update_url' in m), files: hashes };
  if (permissions) hello.granted = await grantedSites(permissions);
  return { id: 0, hello };
}

function pathOf(url) {
  try {
    return new URL(url).pathname;
  } catch {
    return '';
  }
}

function contentType(res) {
  return (res.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
}

// finalURL is where the response came from after redirects, url when
// the response does not say.
function finalURL(res, url) {
  try {
    return new URL(res.url || url);
  } catch {
    return null;
  }
}

// ANTI_BOT_TEXT marks a challenge or anti-bot refusal in a body: a
// Cloudflare interstitial ("Just a moment...") or a JSON refusal naming
// anti-bot rules or a captcha.
const ANTI_BOT_TEXT = /just a moment\.\.\.|cf-challenge|challenge-platform|anti-?bot|captcha/i;

// BODY_TEXT_BYTES caps how much of a body bodyText reads.
const BODY_TEXT_BYTES = 64 * 1024;

// bodyText reads at most BODY_TEXT_BYTES of a copy of res's body, '' when
// it cannot. It streams the copy and stops at the cap, so a large page is
// never buffered whole.
async function bodyText(res) {
  try {
    const body = res.clone().body;
    if (!body) return '';
    const reader = body.getReader();
    const parts = [];
    let total = 0;
    while (total < BODY_TEXT_BYTES) {
      const { done, value } = await reader.read();
      if (done) break;
      const part = value.subarray(0, BODY_TEXT_BYTES - total);
      parts.push(part);
      total += part.length;
    }
    reader.cancel().catch(() => {});
    const bytes = new Uint8Array(total);
    let off = 0;
    for (const part of parts) {
      bytes.set(part, off);
      off += part.length;
    }
    return new TextDecoder().decode(bytes);
  } catch {
    return '';
  }
}

// antiBot reports whether res is an anti-bot page rather than the site's
// answer: Cloudflare's cf-mitigated header, a redirect to Google's
// /sorry/ interstitial, or (when body is read) a challenge marker.
function antiBot(res, url, body) {
  if ((res.headers.get('cf-mitigated') || '') !== '') return true;
  const u = finalURL(res, url);
  if (u && u.pathname.startsWith('/sorry/')) return true;
  return body !== undefined && ANTI_BOT_TEXT.test(body);
}

// check maps an HTTP status to an error class. notFound marks endpoints
// where 404 means the item is gone rather than the API moved. Anti-bot
// pages are blocked whatever their status, except that a 401 stays
// not_logged_in.
async function check(res, url, notFound) {
  const where = `HTTP ${res.status} from ${pathOf(url)}`;
  if (res.status !== 401 && antiBot(res, url)) throw new OpError('blocked', `anti-bot check (${where})`);
  if (res.ok) return;
  if (res.status === 401) throw new OpError('not_logged_in', where);
  if (res.status === 403) {
    if (contentType(res) === 'text/html') throw new OpError('blocked', where);
    if (antiBot(res, url, await bodyText(res))) throw new OpError('blocked', `anti-bot check (${where})`);
    throw new OpError('not_logged_in', where);
  }
  if (res.status === 404 || res.status === 410) throw new OpError(notFound ? 'not_found' : 'endpoint_changed', where);
  if (res.status === 429) throw new OpError('rate_limited', where, retryAfterSeconds(res));
  throw new OpError('http_error', where);
}

// geminiId checks a Gemini conversation id (the URL's hex).
function geminiId(id) {
  if (typeof id !== 'string' || !GEMINI_ID_RE.test(id)) throw bad('invalid Gemini conversation id');
  return id;
}

// parseBatchexecute reads a batchexecute answer: a ")]}'" guard, then
// length-prefixed chunks, one of which holds [["wrb.fr", rpcid,
// "<inner JSON>", ...]]. It returns the decoded inner payload, or null
// when the site answered the rpcid with no payload (an error code in
// place of it). Anything else is endpoint_changed.
export function parseBatchexecute(text, rpcid) {
  if (typeof text !== 'string') throw new OpError('endpoint_changed', `no ${rpcid} answer`);
  const body = text.replace(/^\)\]\}'\s*/, '');
  for (const line of body.split('\n')) {
    const t = line.trim();
    if (!t.startsWith('[')) continue;
    let v;
    try {
      v = JSON.parse(t);
    } catch {
      continue;
    }
    if (!Array.isArray(v)) continue;
    for (const e of v) {
      if (!Array.isArray(e) || e[0] !== 'wrb.fr' || e[1] !== rpcid) continue;
      if (e[2] === null || e[2] === undefined) return null;
      if (typeof e[2] !== 'string') throw new OpError('endpoint_changed', `unexpected ${rpcid} answer`);
      try {
        return JSON.parse(e[2]);
      } catch {
        throw new OpError('endpoint_changed', `malformed ${rpcid} payload`);
      }
    }
  }
  throw new OpError('endpoint_changed', `no ${rpcid} answer`);
}

// geminiImageURLs lists a response candidate's image URLs the way the Go
// side counts them: every string in its arrays, outside its text at
// position 1, that starts with GEMINI_IMAGE_PREFIX, first occurrence
// first, depth first.
export function geminiImageURLs(cand) {
  const out = [];
  const walk = (v) => {
    if (typeof v === 'string') {
      if (v.startsWith(GEMINI_IMAGE_PREFIX) && !out.includes(v)) out.push(v);
    } else if (Array.isArray(v)) {
      for (const e of v) walk(e);
    }
  };
  if (Array.isArray(cand)) cand.forEach((e, i) => i !== 1 && walk(e));
  return out;
}

// geminiImageURL finds image n of response candidate rc in an hNvQHb
// payload (turns at [0], each turn's candidates at [3][0], a candidate's
// id at [0]), and returns it only when it is a plain https URL on the
// image host. Anything else is null.
export function geminiImageURL(inner, rc, n) {
  const turns = Array.isArray(inner) && Array.isArray(inner[0]) ? inner[0] : [];
  for (const t of turns) {
    const cands = Array.isArray(t) && Array.isArray(t[3]) && Array.isArray(t[3][0]) ? t[3][0] : [];
    for (const c of cands) {
      if (!Array.isArray(c) || c[0] !== rc) continue;
      const raw = geminiImageURLs(c)[n];
      if (!raw) return null;
      let u;
      try {
        u = new URL(raw);
      } catch {
        return null;
      }
      if (u.protocol !== 'https:' || u.hostname !== 'lh3.googleusercontent.com' || u.username || u.password || u.port) return null;
      return u.href;
    }
  }
  return null;
}

// decodeImage checks a captured image (base64 from a page) and returns its
// bytes, or null.
export function decodeImage(r) {
  if (!r || r.ok !== true || typeof r.mime !== 'string' || !r.mime.startsWith('image/') || typeof r.data !== 'string') return null;
  if (r.data.length > Math.ceil(MAX_FILE_BYTES / 3) * 4 + 4) return null;
  let bin;
  try {
    bin = atob(r.data);
  } catch {
    return null;
  }
  if (bin.length === 0 || bin.length > MAX_FILE_BYTES) return null;
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return { bytes, mime: r.mime.split(';')[0].trim().toLowerCase() };
}

function b64(bytes) {
  let s = '';
  for (let i = 0; i < bytes.length; i += 0x8000) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  }
  return btoa(s);
}

// createRunner returns the operation runner. sender (send.js) carries out
// the send operations; reload reloads the extension. Either may be absent,
// and its operations then fail as unsupported.
//
// permissions (chrome.permissions) is asked before each operation whether
// its site is granted; without it every site counts as granted.
export function createRunner({ fetch, sender = null, reload = null, permissions = null }) {
  let claudeOrg = null;
  let geminiSession = null;
  // geminiReq is batchexecute's _reqid: a counter, as the app keeps one.
  let geminiReq = Math.floor(Math.random() * 9000) + 1000;
  let reloadPending = false;

  // scheduleReload reloads once the sender is idle, or at the cap after
  // closing its kept tabs. Timers are looked up at call time so tests can
  // mock them.
  function scheduleReload() {
    if (reloadPending) return;
    reloadPending = true;
    const start = Date.now();
    const attempt = async () => {
      const busy = sender && typeof sender.busy === 'function' && sender.busy();
      if (busy) {
        if (Date.now() - start < RELOAD_MAX_WAIT_MS) {
          setTimeout(attempt, RELOAD_RETRY_MS);
          return;
        }
        try {
          if (typeof sender.closeAllKept === 'function') await sender.closeAllKept();
        } catch {
          // Reload regardless.
        }
      }
      reload();
    };
    setTimeout(attempt, 200);
  }

  async function send(url, init) {
    try {
      return await fetch(url, { method: 'GET', cache: 'no-store', ...init });
    } catch {
      throw new OpError('network', `network error for ${pathOf(url)}`);
    }
  }

  // getJSON fetches one of a site's JSON endpoints. An answer that came
  // from another host means the site sent the browser to a sign-in page
  // (the session probes run first, so a logged-out browser never sends).
  async function getJSON(url, init, notFound = false) {
    const res = await send(url, { credentials: 'include', ...init });
    await check(res, url, notFound);
    const at = finalURL(res, url);
    if (at && at.host !== new URL(url).host) throw new OpError('not_logged_in', `redirected to ${at.host}`);
    if (!contentType(res).includes('json')) {
      if (antiBot(res, url, await bodyText(res))) throw new OpError('blocked', `anti-bot check from ${pathOf(url)}`);
      throw new OpError('endpoint_changed', `non-JSON answer from ${pathOf(url)}`);
    }
    try {
      return await res.json();
    } catch {
      throw new OpError('endpoint_changed', `malformed JSON from ${pathOf(url)}`);
    }
  }

  // readCapped reads the body, throwing too_large as soon as it passes
  // MAX_FILE_BYTES rather than after buffering all of it.
  async function readCapped(res) {
    if (!res.body) return new Uint8Array(0);
    const reader = res.body.getReader();
    const parts = [];
    let total = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.length;
      if (total > MAX_FILE_BYTES) {
        reader.cancel().catch(() => {});
        throw new OpError('too_large', `file over ${MAX_FILE_BYTES} bytes`);
      }
      parts.push(value);
    }
    const bytes = new Uint8Array(total);
    let off = 0;
    for (const part of parts) {
      bytes.set(part, off);
      off += part.length;
    }
    return bytes;
  }

  async function emitFile(url, init, emit) {
    const res = await send(url, init);
    await check(res, url, true);
    const declared = Number(res.headers.get('content-length') || 0);
    if (declared > MAX_FILE_BYTES) throw new OpError('too_large', `file over ${MAX_FILE_BYTES} bytes`);
    const mime = contentType(res);
    if (!mime.startsWith('image/') && mime !== 'application/octet-stream') {
      throw new OpError('endpoint_changed', `unexpected file type from ${pathOf(url)}`);
    }
    const bytes = await readCapped(res);
    if (bytes.length === 0) throw new OpError('endpoint_changed', 'empty file');
    emitBytes(bytes, mime, emit);
  }

  function emitBytes(bytes, mime, emit) {
    for (let seq = 0, off = 0; off < bytes.length; seq++, off += CHUNK_BYTES) {
      const end = Math.min(off + CHUNK_BYTES, bytes.length);
      emit({ ok: true, mime, size: bytes.length, chunk: { seq, last: end === bytes.length, data: b64(bytes.subarray(off, end)) } });
    }
  }

  // ChatGPT: the token stays in this function's scope.
  async function chatgptAuth() {
    const s = await getJSON(`${CHATGPT}/api/auth/session`, {});
    if (!s || typeof s.accessToken !== 'string' || s.accessToken === '') {
      throw new OpError('not_logged_in', 'no chatgpt.com session');
    }
    return { credentials: 'include', headers: { Authorization: `Bearer ${s.accessToken}` } };
  }

  // allowedDownload resolves a download_url and says how to fetch it:
  // chatgpt.com URLs with the session, signed file URLs without any
  // credentials. Anything else is refused.
  function allowedDownload(raw) {
    let u;
    try {
      u = new URL(raw, CHATGPT);
    } catch {
      return null;
    }
    if (u.protocol !== 'https:' || u.username || u.password || u.port) return null;
    if (u.hostname === 'chatgpt.com') return { url: u.href, session: true };
    if (u.hostname.endsWith('.oaiusercontent.com')) return { url: u.href, session: false };
    return null;
  }

  async function claudeOrgId() {
    if (claudeOrg) return claudeOrg;
    const orgs = await getJSON(`${CLAUDE}/api/organizations`, {});
    if (!Array.isArray(orgs)) throw new OpError('endpoint_changed', 'unexpected organizations answer');
    if (orgs.length === 0) throw new OpError('not_logged_in', 'no claude.ai organization');
    const chat = orgs.find((o) => o && Array.isArray(o.capabilities) && o.capabilities.includes('chat')) || orgs[0];
    if (!chat || typeof chat.uuid !== 'string' || !ID_RE.test(chat.uuid)) {
      throw new OpError('endpoint_changed', 'unexpected organization id');
    }
    claudeOrg = chat.uuid;
    return claudeOrg;
  }

  // geminiAuth returns the app page's session values: the at token, the
  // build label and the session id. They stay in this closure, are never
  // returned, and are fetched again after GEMINI_SESSION_MS or when
  // fresh is set.
  async function geminiAuth(fresh) {
    if (!fresh && geminiSession && Date.now() - geminiSession.t < GEMINI_SESSION_MS) return geminiSession;
    geminiSession = null;
    const url = `${GEMINI}/app`;
    const res = await send(url, { credentials: 'include' });
    await check(res, url, false);
    const at = finalURL(res, url);
    if (at && at.host !== new URL(url).host) throw new OpError('not_logged_in', `redirected to ${at.host}`);
    let html;
    try {
      html = await res.text();
    } catch {
      throw new OpError('network', 'could not read the Gemini app page');
    }
    const token = /"SNlM0e":"([^"\\]{1,512})"/.exec(html);
    if (!token) throw new OpError('not_logged_in', 'no Gemini session on the app page');
    const bl = /"cfb2h":"([^"\\]{1,256})"/.exec(html);
    if (!bl) throw new OpError('endpoint_changed', 'no build label on the Gemini app page');
    const fsid = /"FdrFJe":"(-?\d{1,32})"/.exec(html);
    geminiSession = { at: token[1], bl: bl[1], fsid: fsid ? fsid[1] : '', t: Date.now() };
    return geminiSession;
  }

  // geminiRPC calls one batchexecute rpcid with payload and returns the
  // decoded inner payload (null when the site sent none). A 400 or 401
  // first fetches the session values again, once.
  async function geminiRPC(rpcid, payload) {
    for (let attempt = 0; ; attempt++) {
      const s = await geminiAuth(attempt > 0);
      const q = new URLSearchParams({ rpcids: rpcid, 'source-path': '/app', bl: s.bl });
      if (s.fsid) q.set('f.sid', s.fsid);
      q.set('hl', 'en');
      q.set('_reqid', String(geminiReq));
      geminiReq += 100000;
      q.set('rt', 'c');
      const url = `${GEMINI}/_/BardChatUi/data/batchexecute?${q}`;
      const body = new URLSearchParams({ 'f.req': JSON.stringify([[[rpcid, JSON.stringify(payload), null, 'generic']]]), at: s.at });
      const res = await send(url, {
        method: 'POST',
        credentials: 'include',
        headers: { 'content-type': 'application/x-www-form-urlencoded;charset=UTF-8' },
        body: body.toString(),
      });
      if ((res.status === 400 || res.status === 401) && attempt === 0) {
        geminiSession = null;
        continue;
      }
      await check(res, url, false);
      const at = finalURL(res, url);
      if (at && at.host !== new URL(url).host) throw new OpError('not_logged_in', `redirected to ${at.host}`);
      let text;
      try {
        text = await res.text();
      } catch {
        throw new OpError('network', `could not read the ${rpcid} answer`);
      }
      return parseBatchexecute(text, rpcid);
    }
  }

  // geminiRead reads conversation id (URL hex) with hNvQHb.
  async function geminiRead(id) {
    const inner = await geminiRPC('hNvQHb', [`c_${geminiId(id)}`, 10, null, 1, [0], [4], null, 1]);
    if (inner === null) throw new OpError('not_found', 'conversation not found');
    return inner;
  }

  const handlers = {
    async 'chatgpt.list'(a) {
      const auth = await chatgptAuth();
      return getJSON(`${CHATGPT}/backend-api/conversations?offset=0&limit=${a.count}&order=updated`, auth);
    },
    async 'chatgpt.detail'(a) {
      const auth = await chatgptAuth();
      return getJSON(`${CHATGPT}/backend-api/conversation/${encodeURIComponent(a.id)}`, auth, true);
    },
    async 'chatgpt.file'(a, emit) {
      const auth = await chatgptAuth();
      const id = encodeURIComponent(a.file_id);
      let metaURL;
      if (a.file_id.startsWith('file-')) {
        metaURL = `${CHATGPT}/backend-api/files/${id}/download`;
      } else {
        const q = new URLSearchParams();
        if (a.conversation_id) q.set('conversation_id', a.conversation_id);
        q.set('inline', 'false');
        metaURL = `${CHATGPT}/backend-api/files/download/${id}?${q}`;
      }
      const meta = await getJSON(metaURL, auth, true);
      if (!meta || meta.status !== 'success' || typeof meta.download_url !== 'string') {
        throw new OpError('endpoint_changed', 'unexpected file download answer');
      }
      const dl = allowedDownload(meta.download_url);
      if (!dl) throw new OpError('endpoint_changed', 'download URL on an unexpected host');
      await emitFile(dl.url, dl.session ? auth : { credentials: 'omit' }, emit);
      return undefined;
    },
    async 'claudeai.list'(a) {
      const org = await claudeOrgId();
      const list = await getJSON(`${CLAUDE}/api/organizations/${org}/chat_conversations?limit=${a.count}`, {});
      return Array.isArray(list) ? list.slice(0, a.count) : list;
    },
    async 'claudeai.detail'(a) {
      const org = await claudeOrgId();
      const id = encodeURIComponent(a.id);
      return getJSON(`${CLAUDE}/api/organizations/${org}/chat_conversations/${id}?tree=True&rendering_mode=messages&render_all_tools=true`, {}, true);
    },
    async 'claudeai.file'(a, emit) {
      const org = await claudeOrgId();
      await emitFile(`${CLAUDE}/api/${org}/files/${encodeURIComponent(a.file_id)}/preview`, { credentials: 'include' }, emit);
      return undefined;
    },
    // The session check runs first, so a logged-out browser never gets a
    // tab and never sends anonymously.
    async 'chatgpt.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      await chatgptAuth();
      return sender.send('chatgpt', a);
    },
    // The read operations keep the organization id cached, so the send
    // drops it and asks claude.ai again: a browser that logged out since
    // the last read must not get a tab.
    async 'claudeai.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      claudeOrg = null;
      await claudeOrgId();
      return sender.send('claudeai', a);
    },
    // The list reads MaZiqc a page at a time, passing each page's token
    // for the next, until it has count conversations or the pages end.
    async 'gemini.list'(a) {
      const pages = [];
      let token = null;
      let seen = 0;
      for (let i = 0; i < GEMINI_MAX_PAGES; i++) {
        const inner = await geminiRPC('MaZiqc', [GEMINI_PAGE_SIZE, token, [0, null, 1]]);
        if (inner === null) break;
        if (!Array.isArray(inner)) throw new OpError('endpoint_changed', 'unexpected MaZiqc payload');
        pages.push(inner);
        seen += Array.isArray(inner[2]) ? inner[2].length : 0;
        token = typeof inner[1] === 'string' && inner[1] !== '' ? inner[1] : null;
        if (!token || seen >= a.count) break;
      }
      return { pages };
    },
    async 'gemini.detail'(a) {
      return geminiRead(a.id);
    },
    // gemini.file: file_id is "<response candidate id>-<n>". The image is
    // captured in the tab the send left open when there is one (the
    // isolated world's fetch, with the page's cookies), else fetched
    // here with the image host's grant. Its <img> is never drawn to a
    // canvas: a cross-origin image taints it.
    async 'gemini.file'(a, emit) {
      const conv = geminiId(a.conversation_id);
      const m = /^([A-Za-z0-9][A-Za-z0-9_-]{0,120})-(\d{1,2})$/.exec(a.file_id);
      if (!m) throw bad('invalid Gemini image id');
      const url = geminiImageURL(await geminiRead(conv), m[1], Number(m[2]));
      if (!url) throw new OpError('not_found', 'image not found in the conversation');
      if (sender && typeof sender.capture === 'function') {
        let got = null;
        try {
          got = decodeImage(await sender.capture('gemini', conv, url, MAX_FILE_BYTES));
        } catch {
          got = null;
        }
        if (got) {
          emitBytes(got.bytes, got.mime, emit);
          return undefined;
        }
      }
      await emitFile(url, { credentials: 'include' }, emit);
      return undefined;
    },
    // The session values are fetched fresh for a send, as for claude.ai.
    async 'gemini.send'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      if (a.conversation_id !== undefined) geminiId(a.conversation_id);
      await geminiAuth(true);
      return sender.send('gemini', a);
    },
    // close touches only tabs a send opened and left open.
    async 'chatgpt.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('chatgpt', a.conversation_id);
    },
    async 'claudeai.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('claudeai', a.conversation_id);
    },
    async 'gemini.close'(a) {
      if (!sender) throw new OpError('unsupported', 'this extension build cannot send');
      return sender.close('gemini', a.conversation_id);
    },
    // The answer goes out first; the reload follows a moment later, or
    // once no send has a tab open (see RELOAD_MAX_WAIT_MS).
    async 'extension.reload'(_a, emit) {
      if (!reload) throw new OpError('unsupported', 'reload is not available');
      emit({ ok: true, result: { reloading: true } });
      scheduleReload();
      return undefined;
    },
  };

  // requireGrant fails permission_missing when op's site's page origins
  // are not granted.
  // Closing a tab the extension opened reaches no site, so it runs
  // regardless: a grant revoked while a reply is read still lets the tab
  // close.
  async function requireGrant(op) {
    const [site, verb] = op.split('.');
    if (!permissions || !Object.hasOwn(SITE_ACCESS, site) || verb === 'close') return;
    if (!(await siteGranted(permissions, site))) {
      throw new OpError('permission_missing', `the extension has no access to ${SITE_ACCESS[site].label}; grant it on the extension's options page`);
    }
  }

  return {
    // run executes one validated operation, calling emit with each
    // response frame (without the request id). It throws OpError.
    async run(op, args, emit) {
      if (!Object.hasOwn(handlers, op)) throw bad('unknown operation');
      await requireGrant(op);
      try {
        const result = await handlers[op](args, emit);
        if (result !== undefined) emit({ ok: true, result });
      } catch (e) {
        if (e instanceof OpError && e.code === 'not_logged_in') claudeOrg = null;
        if (e instanceof OpError && (e.code === 'not_logged_in' || e.code === 'blocked')) geminiSession = null;
        throw e;
      }
    },
  };
}
