// Agent Tincan history bridge: background service worker.
//
// Connects to the native host `tincan history native-host` and answers its
// requests with the fixed operations in ops.js (sends go through send.js).
// On connect it says hello with its version and file hashes, so the host
// can ask for a reload when the unpacked files on disk are newer, and the
// sites it has access to; a grant or revocation (from the options page or
// Chrome's site settings) sends the hello again on the same port. An open
// native port keeps the worker alive; if the host is missing or exits, an
// alarm retries.

import { NATIVE_HOST, createRunner, errorFrame, hashFiles, helloMessage, validate } from './ops.js';
import { createSender } from './send.js';

const RECONNECT_ALARM = 'tincan-reconnect';
const runner = createRunner({
  fetch: (url, init) => fetch(url, init),
  sender: createSender({ tabs: chrome.tabs, scripting: chrome.scripting }),
  reload: () => chrome.runtime.reload(),
  permissions: chrome.permissions,
});
let port = null;
// The files are hashed once, when this worker starts: every hello reports
// the code Chrome loaded, not whatever is on disk at reconnect time, so
// the host can tell when an update is waiting for a reload.
const loadedFiles = hashFiles({ getURL: (f) => chrome.runtime.getURL(f), fetch: (url, init) => fetch(url, init) });

function post(msg) {
  if (!port) return;
  try {
    port.postMessage(msg);
  } catch {
    // The host went away; the alarm reconnects.
  }
}

function requestId(msg) {
  return msg && Number.isSafeInteger(msg.id) && msg.id >= 0 ? msg.id : 0;
}

async function onHostMessage(msg) {
  let req;
  try {
    req = validate(msg);
  } catch (e) {
    post({ id: requestId(msg), ok: false, error: { code: 'bad_request', message: e.message } });
    return;
  }
  try {
    await runner.run(req.op, req.args, (frame) => post({ id: req.id, ...frame }));
  } catch (e) {
    post({ id: req.id, ...errorFrame(e) });
  }
}

function connect() {
  if (port) return;
  let p;
  try {
    p = chrome.runtime.connectNative(NATIVE_HOST);
  } catch {
    return;
  }
  port = p;
  p.onMessage.addListener((msg) => {
    onHostMessage(msg);
  });
  p.onDisconnect.addListener(() => {
    // Reading lastError marks it handled (host not installed, or exited).
    void chrome.runtime.lastError;
    if (port === p) port = null;
  });
  hello(p);
}

// hello tells the host on port p who this worker is and which sites it
// may serve.
function hello(p) {
  loadedFiles
    .then((files) => helloMessage({ manifest: chrome.runtime.getManifest(), files, permissions: chrome.permissions }))
    .then((m) => {
      if (port === p) post(m);
    })
    .catch(() => {});
}

function grantsChanged() {
  if (port) hello(port);
}

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.permissions.onAdded.addListener(grantsChanged);
chrome.permissions.onRemoved.addListener(grantsChanged);
chrome.alarms.create(RECONNECT_ALARM, { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === RECONNECT_ALARM) connect();
});
connect();
