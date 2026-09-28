# Agent Tincan History extension

Manifest V3 extension that lets the `history` agent read ChatGPT and claude.ai
conversations, and the `chatgpt-web` and `claude-web` agents send to them,
through the user's own logged-in Chrome. Its service worker runs a fixed set of
operations (`ops.js`) for the native host `com.agenttincan.history`
(`tincan history native-host`) and returns JSON, with images as base64 in
chunks of at most 384 KiB. It accepts nothing else and never runs code from a
message or a page. The ChatGPT access token is read from `/api/auth/session`
inside the worker and never leaves it.

The send operations (`chatgpt.send`, `claudeai.send`, in `send.js`) open a
background tab of their own, fill the message box through fixed page functions
injected with `chrome.scripting` (message as an argument, isolated world),
click send, and return once the conversation id is in the tab's address. They
never watch the page for the answer: the Go side reads the conversation until
the answer is finished, then calls `chatgpt.close` or `claudeai.close`, which
close only the tab a send left open for that conversation (any such tab is
closed after 10 minutes regardless).
All page selectors are in the `SELECTORS` table in `send.js`; see
docs/adapters/web-agents.md.

## Site access and the options page

`SITE_ACCESS` in `ops.js` lists each site's origins, keyed by its op prefix,
and among them its page origins (`pageOrigins`). Before any operation except
close, the worker asks `chrome.permissions.contains` for the site's page
origins and fails with `permission_missing` (no tab, no fetch) when they are
not granted. The other origins (ChatGPT's `*.oaiusercontent.com` file host)
are not checked up front: withholding one leaves list and read working, and
only a file fetch that reaches it fails, as any fetch to an ungranted host
does. Close runs
regardless, so a grant revoked while a reply is read still lets the tab close.
ChatGPT and claude.ai are required `host_permissions`, as before, so an
upgrade asks for nothing new; the owner can still withhold them in Chrome's
site access settings. Sites added later go under `optional_host_permissions`
(none yet, so the manifest has no such key) and are granted from the options
page: `options.html` and `options.js`, opened from `chrome://extensions` >
Agent Tincan History > Details > Extension options. It lists every site in
`SITE_ACCESS`, shows whether all its origins are granted (a site with only
its page origins granted shows as granted with images and files needing
file access, and a "Grant file access" button), and its Grant button calls
`chrome.permissions.request` for all of the site's origins straight from the
click (Chrome allows the
request only during a user gesture). The page is built with `textContent`, has
no inline script, and never runs in a site's page.

The hello lists the granted sites (`granted`, op prefixes), and the worker
says hello again on `chrome.permissions.onAdded` and `onRemoved`, so the host
learns a grant without a reconnect. Only the newest hello started is posted,
so one built before a later change and finishing after it is dropped rather
than overwriting the newer grant list the host keeps. The host treats a hello without `granted`
(an older extension) as ChatGPT and claude.ai only. `tincan web serve` asks
the host (`host.status`, answered by the host itself) at startup. When the
extension is connected and reports its site ungranted, it logs that once
(naming the options page) and waits, asking again every minute, until the
grant appears or the extension goes away; it does not exit, so the service
manager never restarts it in a loop.

## Failure codes

Besides `not_logged_in`, `not_found`, `rate_limited` and the rest, the fetch
check reports anti-bot pages as `blocked`: a Cloudflare challenge (the
`cf-mitigated` header, or a "Just a moment..." page), a 403 JSON refusal that
names anti-bot rules or a captcha, and a redirect to Google's `/sorry/`
interstitial. A plain 401 stays `not_logged_in`. A session probe (ChatGPT's
`/api/auth/session`, claude.ai's organizations) that was redirected to
another host is a sign-in page, so it is `not_logged_in` and no send follows.

On connect the worker sends the host a hello with its version, its granted
sites and the sha256 of each file, hashed once when the worker started (so it describes the code
Chrome loaded). When `tincan history install --extension-dir` (or a run from
the repo checkout) told the host where the unpacked files are, and they
differ, the host sends `extension.reload` and the worker calls
`chrome.runtime.reload()`, so updates need no Reload click after the first
load. The host compares again every 10 minutes while the worker stays
connected, so an update that lands mid-session is picked up too. The reload waits while a send has a tab open (checking every 5 seconds,
up to 5 minutes; at that cap it closes finished sends' tabs and reloads
anyway).

## Extension id

`manifest.json` carries a `key`: an RSA-2048 public key, base64 DER
SubjectPublicKeyInfo. Chrome derives the id from it (sha256 of the DER bytes,
first 16 bytes as hex, digits 0-f mapped to letters a-p), so an unpacked load
always gets `ciejooalclcpgpapboofdbbddphldhnh` (`history.DefaultExtensionID`,
checked by `TestExtensionIDFromManifestKey`). Only the public key is in the
repo; loading unpacked does not need the private key.

To rotate the key: `openssl genrsa -out key.pem 2048`, then
`openssl rsa -in key.pem -pubout -outform DER | base64` into `key`, update
`DefaultExtensionID`, and keep `key.pem` out of the repo. A Chrome Web Store
listing may assign its own id; pass it with
`tincan history install --extension-id <id>`.

## Develop

- `make extension-test` runs the worker tests (`node --test`, no dependencies).
- `make extension` writes `dist/tincan-history-extension.zip`.
- Load unpacked from `chrome://extensions`, then run `tincan history install`
  so Chrome can start the native host.
