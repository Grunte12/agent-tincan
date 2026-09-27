// Send operations for the Agent Tincan web agents (chatgpt.send,
// claudeai.send, gemini.send) and the matching close operations
// (chatgpt.close, claudeai.close, gemini.close), plus the in-page image
// capture gemini.file asks for (capture).
//
// ChatGPT and claude.ai guard their send endpoints with anti-bot tokens, so
// instead of calling them this module drives the real page UI in a
// background tab the extension opens itself (never one of the user's tabs):
//
//   1. open https://chatgpt.com/ (or /c/<id>), https://claude.ai/new (or
//      /chat/<id>) or https://gemini.google.com/app (or /app/<id>) with
//      chrome.tabs.create({active: false});
//   2. inject the fixed page functions below with chrome.scripting
//      (isolated world, func + args only) to fill the composer, verify the
//      text landed, and click send;
//   3. once the page took the message, wait only for the conversation id in
//      the tab URL (already known when continuing a conversation) and
//      return {conversation_id, url, submitted_at}.
//
// The send does not watch the page for the reply: a background tab is
// throttled, so page-side signals are unreliable. The Go side reads the
// conversation through the existing detail operation until the reply is
// finished, then calls the close operation with the conversation id. The
// tab stays open until then because closing it may stop claude.ai from
// finishing the reply. A tab nobody closes is closed after keepMs anyway.
// A send that fails closes its tab at once.
//
// While that tab is still open, capture fetches one of Gemini's images
// from inside it (the isolated world's fetch, with the page's cookies), so
// the image host sees the same request the page's own would make. It never
// draws the page's <img> onto a canvas: a cross-origin image taints it.
//
// The message is data only: it is passed as an argument to a fixed function
// and inserted as text. Nothing from a message or a page is ever executed.
// Selectors drift, so every one lives in SELECTORS, each with fallbacks.

import { OpError, GEMINI_IMAGE_PREFIX, MAX_FILE_BYTES } from './ops.js';

export const SEND_TIMEOUT_MS = 5 * 60 * 1000;
// ID_WAIT_MS bounds the wait for the conversation id after the send.
export const ID_WAIT_MS = 60 * 1000;
// KEEP_TAB_MS is how long a finished send's tab may stay open waiting for
// its close operation.
export const KEEP_TAB_MS = 10 * 60 * 1000;

// SELECTORS is the one table of page selectors, tried in order. login and
// loginPaths mean the page is logged out. stop and streaming refuse a send
// into a conversation that is still answering, and with assistant and user
// they help confirm that the page took the message. None of them is ever
// used to decide that a reply is finished.
export const SELECTORS = Object.freeze({
  chatgpt: Object.freeze({
    composer: ['#prompt-textarea', 'div[contenteditable="true"][id="prompt-textarea"]', 'textarea[data-id="root"]', 'form div[contenteditable="true"]'],
    send: ['[data-testid="send-button"]', '#composer-submit-button', 'button[aria-label*="Send"]'],
    stop: ['[data-testid="stop-button"]', 'button[aria-label*="Stop"]'],
    streaming: ['.result-streaming'],
    assistant: ['[data-message-author-role="assistant"]'],
    user: ['[data-message-author-role="user"]'],
    login: ['[data-testid="login-button"]', 'a[href*="/auth/login"]'],
    loginPaths: ['/auth/login', '/log-in'],
  }),
  claudeai: Object.freeze({
    composer: ['div[contenteditable="true"].ProseMirror', 'fieldset div[contenteditable="true"]', '[contenteditable="true"][aria-label*="prompt" i]', 'div[contenteditable="true"]'],
    send: ['button[aria-label="Send message"]', 'button[aria-label*="Send"]', 'fieldset button[type="submit"]'],
    stop: ['button[aria-label="Stop response"]', 'button[aria-label*="Stop"]'],
    streaming: ['[data-is-streaming="true"]'],
    assistant: ['[data-is-streaming]', '.font-claude-response', '.font-claude-message', '[data-testid="assistant-message"]'],
    user: ['[data-testid="user-message"]'],
    login: ['a[href="/login"]', 'input[type="email"]'],
    loginPaths: ['/login', '/logout'],
  }),
  // Gemini's composer is a Quill editor inside rich-textarea; the send
  // button only appears once there is text.
  gemini: Object.freeze({
    composer: ['div.ql-editor[aria-label="Enter a prompt for Gemini"]', 'rich-textarea div.ql-editor[contenteditable="true"]', 'div.ql-editor[contenteditable="true"]'],
    send: ['button[aria-label*="Send" i]', 'button.send-button'],
    stop: ['button[aria-label*="Stop" i]'],
    streaming: [],
    assistant: ['model-response', 'message-content'],
    user: ['user-query'],
    login: ['a[href*="accounts.google.com/ServiceLogin"]', 'a[href*="accounts.google.com/v3/signin"]'],
    loginPaths: [],
  }),
});

// SITES says where each site's pages are and how to read a conversation id
// from a URL.
export const SITES = Object.freeze({
  chatgpt: Object.freeze({
    newURL: 'https://chatgpt.com/',
    convURL: (id) => `https://chatgpt.com/c/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/chatgpt\.com\/(?:g\/[A-Za-z0-9_-]+\/)?c\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
  }),
  claudeai: Object.freeze({
    newURL: 'https://claude.ai/new',
    convURL: (id) => `https://claude.ai/chat/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/claude\.ai\/chat\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})(?:[/?#]|$)/,
  }),
  // A Gemini id is the hex in /app/<id>; a Gem's chat shows it as
  // /gem/<name>/<id>, another signed-in account under /u/<n>/.
  gemini: Object.freeze({
    newURL: 'https://gemini.google.com/app',
    convURL: (id) => `https://gemini.google.com/app/${encodeURIComponent(id)}`,
    idFrom: /^https:\/\/gemini\.google\.com\/(?:u\/\d{1,2}\/)?(?:app|gem\/[A-Za-z0-9_-]{1,128})\/([0-9a-f]{8,64})(?:[/?#]|$)/,
  }),
});

// ---- Page functions. Chrome serializes each one and runs it in the tab's
// isolated world, so they must be self-contained: no closures over module
// state, only their arguments. They return plain data.

export function pageProbe(sel) {
  const q = (list) => {
    for (const s of list) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch {
        // A selector the page's engine rejects is skipped.
      }
    }
    return null;
  };
  const qa = (list) => {
    for (const s of list) {
      try {
        const els = document.querySelectorAll(s);
        if (els && els.length) return Array.from(els);
      } catch {
        // Skipped, as above.
      }
    }
    return [];
  };
  const textOf = (el) => (el ? String(el.innerText ?? el.textContent ?? '') : '');
  const path = String(location.pathname || '');
  const composer = q(sel.composer);
  const draft = composer ? (composer.tagName === 'TEXTAREA' ? String(composer.value ?? '') : textOf(composer)) : '';
  return {
    href: String(location.href || ''),
    loggedOut: Boolean(q(sel.login)) || sel.loginPaths.some((p) => path === p || path.startsWith(p + '/')),
    composer: Boolean(composer),
    composerEmpty: draft.trim() === '',
    generating: Boolean(q(sel.stop)) || Boolean(q(sel.streaming)),
    assistantCount: qa(sel.assistant).length,
    userCount: qa(sel.user).length,
  };
}

// pageFill puts message into the composer and checks that it landed. It
// tries typing (execCommand insertText), then a paste event, then setting
// the text directly, clearing the composer between attempts.
export function pageFill(sel, message) {
  const q = (list) => {
    for (const s of list) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch {
        // Skipped.
      }
    }
    return null;
  };
  const composer = q(sel.composer);
  if (!composer) return { ok: false, code: 'composer_not_found' };
  const isTextarea = composer.tagName === 'TEXTAREA';
  const read = () => (isTextarea ? String(composer.value ?? '') : String(composer.innerText ?? composer.textContent ?? ''));
  // Editors turn blank lines into paragraphs and may eat markdown marks, so
  // the check ignores whitespace and those marks.
  const norm = (s) => String(s).replace(/[\s#*`>_-]+/g, '');
  const landed = () => norm(read()) === norm(message);
  const clear = () => {
    if (read().trim() === '') return;
    if (isTextarea) {
      composer.value = '';
      composer.dispatchEvent(new Event('input', { bubbles: true }));
      return;
    }
    try {
      document.execCommand('selectAll', false);
      document.execCommand('delete', false);
    } catch {
      // Fall through to the direct reset.
    }
    if (read().trim() !== '') {
      composer.textContent = '';
      composer.dispatchEvent(new Event('input', { bubbles: true }));
    }
  };
  const attempts = [
    () => document.execCommand('insertText', false, message),
    () => {
      if (typeof DataTransfer === 'undefined' || typeof ClipboardEvent === 'undefined') return false;
      const dt = new DataTransfer();
      dt.setData('text/plain', message);
      return composer.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    },
    () => {
      if (isTextarea) {
        const setter = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(composer), 'value')?.set;
        if (setter) setter.call(composer, message);
        else composer.value = message;
      } else {
        composer.textContent = message;
      }
      return composer.dispatchEvent(new Event('input', { bubbles: true }));
    },
  ];
  const methods = ['insertText', 'paste', 'set'];
  for (let i = 0; i < attempts.length; i++) {
    composer.focus();
    clear();
    try {
      attempts[i]();
    } catch {
      // Try the next method.
    }
    if (landed()) return { ok: true, method: methods[i] };
  }
  clear();
  return { ok: false, code: 'send_failed', message: 'the text did not land in the composer' };
}

// pageSubmit clicks the send button, or presses Enter in the composer when
// no send button exists. A disabled button is reported so the caller can
// retry.
export function pageSubmit(sel) {
  const q = (list) => {
    for (const s of list) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch {
        // Skipped.
      }
    }
    return null;
  };
  const btn = q(sel.send);
  if (btn) {
    const disabled = btn.disabled === true || (btn.getAttribute && btn.getAttribute('aria-disabled') === 'true');
    if (disabled) return { ok: false, code: 'disabled' };
    btn.click();
    return { ok: true, how: 'button' };
  }
  const composer = q(sel.composer);
  if (!composer) return { ok: false, code: 'composer_not_found' };
  composer.focus();
  composer.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true, cancelable: true }));
  return { ok: true, how: 'enter' };
}

// pageFetchImage fetches url with the page's cookies and returns it as
// base64, or why it could not. Only an image under maxBytes is returned.
export async function pageFetchImage(url, maxBytes) {
  try {
    const res = await fetch(url, { credentials: 'include', cache: 'no-store' });
    if (!res.ok) return { ok: false, status: res.status };
    const mime = String(res.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
    if (!mime.startsWith('image/')) return { ok: false, code: 'not_image' };
    const buf = new Uint8Array(await res.arrayBuffer());
    if (buf.length === 0 || buf.length > maxBytes) return { ok: false, code: 'size' };
    let bin = '';
    for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode.apply(null, buf.subarray(i, i + 0x8000));
    return { ok: true, mime, data: btoa(bin) };
  } catch {
    return { ok: false, code: 'network' };
  }
}

// ---- The sender, run in the service worker.

function str(v, n) {
  return typeof v === 'string' ? v.slice(0, n) : '';
}

// cleanProbe keeps only the expected fields of a page answer, with types
// checked: a page is not trusted to shape the worker's data.
function cleanProbe(r) {
  const o = r && typeof r === 'object' ? r : {};
  return {
    href: str(o.href, 4096),
    loggedOut: o.loggedOut === true,
    composer: o.composer === true,
    composerEmpty: o.composerEmpty === true,
    generating: o.generating === true,
    assistantCount: Number.isSafeInteger(o.assistantCount) ? o.assistantCount : 0,
    userCount: Number.isSafeInteger(o.userCount) ? o.userCount : 0,
  };
}

// elsewhere returns url as a URL when it is an http(s) address on another
// host than site's, else null (about:blank and the like while loading).
function elsewhere(url, site) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return null;
  return u.host === new URL(site).host ? null : u;
}

// createSender returns {send(site, args), close(site, conversationId),
// capture(site, conversationId, url), busy(), closeAllKept()}.
// tabs and scripting are chrome.tabs and chrome.scripting (or fakes). Sends
// to one site run one at a time, each in its own background tab. A
// successful send leaves its tab open for close (or the keepMs timer).
export function createSender({
  tabs,
  scripting,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  now = () => Date.now(),
  setTimer = (fn, ms) => setTimeout(fn, ms),
  clearTimer = (t) => clearTimeout(t),
  timeoutMs = SEND_TIMEOUT_MS,
  pollMs = 1000,
  loadMs = 45000,
  sendConfirmMs = 15000,
  idWaitMs = ID_WAIT_MS,
  keepMs = KEEP_TAB_MS,
}) {
  // owned: tabs being driven by a send, the only ones that may be
  // scripted. kept: finished sends' tabs, tab id -> {site, id, timer},
  // the only ones close may remove.
  const owned = new Set();
  const kept = new Map();
  const queues = {};
  // inflight counts sends accepted and not yet settled, queued ones too.
  let inflight = 0;

  // inject runs func in tabId's isolated world. Only a tab a send is
  // driving may be scripted, or, with keptOK, a finished send's tab.
  async function inject(tabId, func, args, keptOK = false) {
    if (!owned.has(tabId) && !(keptOK && kept.has(tabId))) throw new OpError('internal', 'refusing to script a tab the extension did not open');
    let res;
    try {
      res = await scripting.executeScript({ target: { tabId }, world: 'ISOLATED', func, args });
    } catch (e) {
      throw new OpError('send_failed', 'could not reach the page: ' + String((e && e.message) || e).slice(0, 200));
    }
    return Array.isArray(res) && res[0] ? res[0].result : undefined;
  }

  async function tabURL(tabId) {
    try {
      const t = await tabs.get(tabId);
      return { url: str(t && t.url, 4096), status: str(t && t.status, 32) };
    } catch {
      throw new OpError('send_failed', 'the worker tab was closed');
    }
  }

  async function removeTab(tabId) {
    try {
      await tabs.remove(tabId);
    } catch {
      // Already closed.
    }
  }

  function keep(tabId, site, id) {
    const timer = setTimer(() => {
      if (kept.get(tabId)?.timer !== timer) return;
      kept.delete(tabId);
      removeTab(tabId);
    }, keepMs);
    kept.set(tabId, { site, id, timer });
  }

  async function closeKept(match) {
    let closed = 0;
    for (const [tabId, k] of [...kept]) {
      if (!match(k)) continue;
      kept.delete(tabId);
      clearTimer(k.timer);
      await removeTab(tabId);
      closed++;
    }
    return { closed };
  }

  async function run(site, args) {
    const cfg = SITES[site];
    const sel = SELECTORS[site];
    if (!cfg || !sel) throw new OpError('bad_request', 'unknown site');
    const start = now();
    const deadline = start + timeoutMs;
    const late = () => now() >= deadline;
    const existing = args.conversation_id && !args.new_chat ? args.conversation_id : '';
    const target = existing ? cfg.convURL(existing) : cfg.newURL;

    const tab = await tabs.create({ url: target, active: false });
    if (!tab || !Number.isSafeInteger(tab.id)) throw new OpError('send_failed', 'could not open a tab');
    owned.add(tab.id);
    // urlId is the conversation id in the tab's address right now, '' when
    // there is none.
    const urlId = async () => {
      const m = cfg.idFrom.exec((await tabURL(tab.id)).url);
      return m ? m[1] : '';
    };
    let done = false;
    try {
      // 1. Page load, then a composer (or a login page).
      const loadBy = Math.min(deadline, start + loadMs);
      let page = null;
      for (;;) {
        const t = await tabURL(tab.id);
        // The site sent the tab to another host: a sign-in page, or
        // Google's /sorry/ anti-bot page. Nothing is typed there.
        const away = elsewhere(t.url, cfg.newURL);
        if (away) throw new OpError(away.pathname.startsWith('/sorry/') ? 'blocked' : 'not_logged_in', `the page went to ${away.host}`);
        if (t.status === 'complete') {
          page = cleanProbe(await inject(tab.id, pageProbe, [sel]));
          if (page.loggedOut) throw new OpError('not_logged_in', `logged out of ${new URL(cfg.newURL).host}`);
          if (page.composer) break;
        }
        if (now() >= loadBy) {
          if (late()) throw new OpError('timeout', 'the page did not load in time');
          throw new OpError('composer_not_found', 'no message box on the page');
        }
        await sleep(pollMs);
      }
      if (existing) {
        const m = cfg.idFrom.exec(page.href);
        if (!m || m[1] !== existing) throw new OpError('not_found', 'conversation not found');
      }
      // A conversation still answering an earlier message will not take
      // another (the site trades its send button for a stop button), and
      // the answering itself would look like the page taking this one. So
      // it is refused before anything is typed, and again right before the
      // click, in case an answer started meanwhile.
      const answering = () => new OpError('send_failed', 'the conversation is still answering an earlier message');
      if (page.generating) throw answering();

      // 2. Fill and verify.
      const fill = await inject(tab.id, pageFill, [sel, args.message]);
      if (!fill || fill.ok !== true) {
        const code = fill && fill.code === 'composer_not_found' ? 'composer_not_found' : 'send_failed';
        throw new OpError(code, str(fill && fill.message, 200) || 'could not fill the message box');
      }

      // 3. Send: retry while the button is disabled, then confirm the page
      // took the message. The page is probed right before each click, so
      // the confirmation compares with the page as it was then, not as it
      // was when the tab opened. An existing conversation's address is
      // checked then too: a deleted conversation shows its composer first
      // and redirects a moment later, and the message must not go into
      // whatever page is there by then. A new chat's URL gaining an id
      // also confirms the send.
      const confirmBy = Math.min(deadline, now() + sendConfirmMs);
      let submittedAt;
      let base;
      for (;;) {
        if (existing && (await urlId()) !== existing) throw new OpError('not_found', 'conversation not found');
        base = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (base.loggedOut) throw new OpError('not_logged_in', 'logged out while sending');
        if (base.generating) throw answering();
        submittedAt = now();
        const r = await inject(tab.id, pageSubmit, [sel]);
        if (r && r.ok === true) break;
        if (r && r.code === 'composer_not_found') throw new OpError('composer_not_found', 'the message box went away');
        if (now() >= confirmBy) throw new OpError('send_failed', 'the send button stayed disabled');
        await sleep(pollMs);
      }
      for (;;) {
        await sleep(pollMs);
        if (!existing && (await urlId())) break;
        const p = cleanProbe(await inject(tab.id, pageProbe, [sel]));
        if (p.loggedOut) throw new OpError('not_logged_in', 'logged out while sending');
        if (p.userCount > base.userCount || p.generating || p.assistantCount > base.assistantCount) break;
        if (now() >= confirmBy) throw new OpError('send_failed', 'the page did not take the message');
      }

      // 4. The conversation id, from the tab URL as it is now: a new chat's
      // once it appears, or wherever the site put the message if it moved
      // an existing conversation after the click. Nothing waits for the
      // reply.
      let id = await urlId();
      const idBy = Math.min(deadline, now() + idWaitMs);
      while (!id) {
        if (now() >= idBy) throw new OpError('timeout', 'the message was sent but no conversation id appeared in time');
        await sleep(pollMs);
        id = await urlId();
      }
      done = true;
      keep(tab.id, site, id);
      return { conversation_id: id, url: cfg.convURL(id), submitted_at: submittedAt };
    } finally {
      owned.delete(tab.id);
      if (!done) await removeTab(tab.id);
    }
  }

  return {
    // send runs after any earlier send to the same site has finished.
    send(site, args) {
      const prev = queues[site] || Promise.resolve();
      inflight++;
      const p = prev
        .catch(() => {})
        .then(() => run(site, args))
        .finally(() => {
          inflight--;
        });
      queues[site] = p;
      return p;
    },
    // close closes the tabs a finished send to conversationId left open,
    // and only those. It reports how many it closed.
    async close(site, conversationId) {
      return closeKept((k) => k.site === site && k.id === conversationId);
    },
    // capture fetches an image from inside the tab a finished send to
    // conversationId left open (pageFetchImage), for the image host that
    // expects the page's own request. It returns the page's answer
    // ({ok, mime, data}) or null when there is no such tab or the page
    // could not fetch it; it never throws. Only Gemini image URLs are
    // fetched.
    async capture(site, conversationId, url, maxBytes = MAX_FILE_BYTES) {
      if (typeof url !== 'string' || !url.startsWith(GEMINI_IMAGE_PREFIX)) return null;
      const entry = [...kept].find(([, k]) => k.site === site && k.id === conversationId);
      if (!entry) return null;
      try {
        const r = await inject(entry[0], pageFetchImage, [url, maxBytes], true);
        return r && r.ok === true ? r : null;
      } catch {
        return null;
      }
    },
    // busy reports whether a send is queued or driving a tab, or a
    // finished send's tab is still waiting for its close: state a reload
    // would lose.
    busy() {
      return inflight > 0 || owned.size > 0 || kept.size > 0;
    },
    // closeAllKept closes every tab finished sends left open (used before
    // a forced reload, which would otherwise orphan them).
    async closeAllKept() {
      return closeKept(() => true);
    },
  };
}
