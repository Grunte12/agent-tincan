# Web agents: ChatGPT, Claude, Grok and Gemini as teammates

A web agent makes chatgpt.com, claude.ai, grok.com or gemini.google.com a teammate. Another agent asks it something with `tincan ask chatgpt-web "..."` and gets ChatGPT's answer back as the reply, with any images ChatGPT generated attached. `claude-web` does the same with Claude on claude.ai, `grok-web` with Grok on grok.com, and `gemini-web` with Gemini on gemini.google.com (see [Gemini](#gemini) for what differs there, including the account risk and what Gemini can read).

It is a Go service, `tincan web serve --site chatgpt` (or `--site claude-ai`, `--site grok`, `--site gemini`), running on your Mac next to Chrome. It is not a model. For each request it:

1. Checks access. By default any agent joined to your relay may ask. If you wrote an allowlist file, every agent in the request's chain, as the relay recorded it, must be on it; otherwise it declines and names the agent.
2. Reads the optional threading line (below). The rest of the body is the message, sent as is.
3. Has the Tincan Chrome extension type the message into the site, in a background tab the extension opens itself. The extension answers as soon as the message is sent and the conversation id is in the tab's address.
4. Reads the conversation through the same detail operation the history agent uses until the reply is finished (bounded by the request timeout, 8 minutes), then replies with the answer text and the generated images, fetched through the file operation, as attachments. Then it has the extension close the tab. The first read is 5 seconds after the send, then the reads back off: 5, 8 and 12 seconds apart, then every 20 seconds. Every read is a request on your account, so it never polls faster than that.

It handles one request at a time. The extension also runs one send per site at a time. While a request waits for its answer, the agent refreshes its relay presence every 30 seconds with a peek that claims nothing, so it does not show offline; requests that arrive meanwhile stay queued for the next one.

## Rate limits

If the site answers HTTP 429, the agent waits the site's `Retry-After` (the extension reports it), or backs off from 30 seconds, doubling up to 5 minutes. It never retries at the normal cadence. If that wait would outlast the request's 8 minutes, the request fails with "ChatGPT is rate-limiting this account right now. The message was sent to ChatGPT (conversation <id>); ask for the reply later instead of sending it again." (or claude.ai, Grok or Gemini): the message already went through, so sending it again would only duplicate it. While the cooldown runs, the next request fails at once, before anything is sent, with "ChatGPT is rate-limiting this account right now; try again later" instead of asking the site again. The native host keeps the same cooldown for every reader and agent that goes through it, so history reads also stop hitting the site; a history read that meets a 429 fails at once with that message, without retrying. An HTTP 5xx while waiting also backs off (doubling, up to 2 minutes).

Each site has its own cooldown: a rate limit on one never holds back another. Gemini also cools down for 5 minutes after Google shows its anti-bot page (`/sorry/`), so the agent does not keep tripping it on your account; while that runs, requests fail at once with the anti-bot reply instead of asking Google again. ChatGPT and claude.ai start no cooldown on an anti-bot answer.

Grok can also end an answer on the plan's usage limit instead of answering with a 429: the finished response carries a stream error naming a rate or usage limit. The agent treats that like a 429 it cannot wait out: the request fails with "Grok is rate-limiting this account right now. The message was sent to Grok (conversation <id>); ask for the reply later instead of sending it again.", and Grok is left alone for 15 minutes, during which requests to grok-web fail at once without sending.

## What it does in your browser

This types into your logged-in ChatGPT, Claude, Grok or Gemini account, as you. The messages and answers show up in that site's history like any chat you had yourself, count against your plan's usage, and follow the site's own settings (memory, custom instructions, model choice).

Account risk: xAI's terms prohibit automated access to Grok. grok-web acts as you, on your own account, one request at a time and at the human-paced cadence below, through the same page a person uses; it does not scrape, bulk-export or share the account. xAI can still limit or suspend an account it believes is automated, so turn grok-web on only if you accept that.

The extension opens a new background tab (`chrome.tabs.create` with `active: false`), fills the message box, clicks send, and reads the conversation id from the tab's address (at most 60 seconds). It does not watch the page for the answer: Chrome throttles background tabs, so page signals are unreliable there. The tab stays open while the site writes the answer, because closing it may stop claude.ai from finishing, and the web agent closes it with the fixed `chatgpt.close`, `claudeai.close`, `grok.close` or `gemini.close` operation once the answer is read or the wait gives up. That operation closes only a tab a send opened for that conversation. A tab nobody closes is closed after 10 minutes; a send that fails closes its tab at once. It never touches a tab you opened. Chrome is never quit or restarted. The message is passed to a fixed function in the extension as data and inserted as text; nothing in it is ever run as code.

Why a tab and not an API call: all four sites protect their send endpoints with anti-bot tokens that only the real page can produce (grok.com's send carries a per-request `x-statsig-id`). Driving the page is what survives that.

Reads are plain JSON requests from the extension's service worker with your cookies. For grok.com they are `GET /rest/app-chat/conversations` (the list), `GET /rest/app-chat/conversations/<id>/response-node?includeThreads=true` (the message tree and what is still being written) and `POST .../load-responses` (the message bodies); none of them needs a page-set header. If grok.com ever refuses reads from the extension's origin, the one function that fetches them (`grokJSON` in `extension/ops.js`) is where a fixed read in an extension-opened grok.com tab's isolated world would go; nothing ever runs in the page's own JavaScript.

### Grok: grant it first

grok.com is an optional site: the extension has no access to it until you grant it. Open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant for Grok; Chrome asks for grok.com and its image host, assets.grok.com, together. Chrome takes the grant only from that click, so it is a step for you, not for an agent. Until then every grok-web request is answered with that step, and while the extension is connected `tincan web serve --site grok` waits for the grant, checking again every minute (see the grant reply below). grok.com also lets a logged-out browser chat anonymously, so before opening a tab the extension reads your conversation list, and the send tab then checks the page for grok.com's sign-in link or `/sign-in` address before typing; a logged-out browser fails at one of the two and never sends.

## Allowlist

By default there is no allowlist file and every agent joined to your relay may ask, so any agent on your mesh can act as you in ChatGPT, Claude, Grok or Gemini (and, through Gemini, read what Gemini can read; see [Gemini](#gemini)). The startup log says `allowlist: all joined agents (no file at ~/.config/tincan/chatgpt-web-allow.txt)`.

To restrict it, write the web agent's own file, `~/.config/tincan/chatgpt-web-allow.txt`, `~/.config/tincan/claude-web-allow.txt`, `~/.config/tincan/grok-web-allow.txt` or `~/.config/tincan/gemini-web-allow.txt`, with one agent name per line (commas and spaces also separate names, `#` starts a comment). It works exactly like the history allowlist:

- It is reread for every request.
- A file of names allows only those names. A `*` entry means every joined agent, the same as no file. An empty file allows nobody.
- An unreadable file, or an entry that is neither `*` nor a plain agent name, declines everyone (and stops the service from starting).
- A file of names covers the whole chain. If muse asks codex and codex asks chatgpt-web while handling muse's request, a file that lists codex but not muse declines it because of muse. The chain and sender come from the relay, never from the request body.

## Threading

The first line of a request can pick the conversation:

```
new chat
What are three names for a fox mascot?
```

```
conversation: 6a1f0c2e-1111-4a2b-9c3d-000000000001
Make the second one shorter.
```

`conversation:` takes an id or a conversation URL (`https://chatgpt.com/c/<id>`, `https://claude.ai/chat/<id>`, `https://grok.com/c/<id>`, `https://gemini.google.com/app/<id>`, also `/u/<n>/app/<id>` and `/gem/<name>/<id>`). Without either line, the message continues the conversation this asker used last with this agent, or starts one if there is none. Each asker has its own thread: grokbot's follow-ups never land in codex's conversation. The ids are kept in `~/.config/tincan/<agent>-state.json` (mode 0600; ids only, never messages). If a remembered conversation was deleted, the message goes to a new chat and the reply says so. An explicit `conversation:` id that is gone is an error.

Every conversation the agent sends into is also added to `~/.config/tincan/web-agent-<site>-conversations.json` (mode 0600; ids and times only, newest 1000 kept), so the history agent leaves these chats out when you ask what you last asked ChatGPT, Claude, Grok or Gemini.

Every reply ends with the conversation id, for example `ChatGPT conversation: 6a1f0c2e-...` or `Grok conversation: 0e1d0000-...`, so the asker can come back to it.

## Replies

- The answer text, capped at 64 KB. A longer answer is cut and the reply says how much is shown.
- Images the assistant generated in that turn, as relay attachments (up to 8). The reply says how many were attached, or why none were. Grok's images are fetched from assets.grok.com by the extension, which looks each URL up again from the conversation by response id and index (no URL is ever passed to it); an image it cannot fetch is left out and the reply says "The images could not be attached." Grok videos are not attached.
- Messages are capped at 32 KB. A longer one is refused, not cut.
- If the answer finished but the conversation could not be read back, the request fails saying the message was sent and naming the conversation; the answer is there on the site.

## Install

The web agents use the Tincan Chrome extension and its native host, the same ones the history agent uses. If you already run `history`, that part is done. Otherwise load the extension first as described in [history.md](history.md#install) (unzip `tincan-history-extension.zip` into a folder you keep, then Load unpacked in `chrome://extensions` with Developer mode on).

```bash
tincan invite chatgpt-web --kind chatgpt-web          # on an admin device
TINCAN_CONFIG=~/.config/tincan/chatgpt-web.json tincan join <code> --relay http://tincan-relay
tincan history install --no-service --extension-dir ~/tincan-extension   # skip if history is installed
tincan web install --site chatgpt
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.web.chatgpt.plist
```

For Claude, use `claude-web`, `--kind claude-web`, `~/.config/tincan/claude-web.json` and `--site claude-ai` (plist `com.agenttincan.web.claude-ai.plist`). For Grok, use `grok-web`, `--kind grok-web`, `~/.config/tincan/grok-web.json` and `--site grok` (plist `com.agenttincan.web.grok.plist`), and grant grok.com on the extension's options page first (above). A relay older than the `grok-web` kind refuses `--kind grok-web`, and the CLI says so; upgrade the relay, or invite `grok-web` without `--kind` and run `tincan onboard --kind grok-web=grok-web` for its block. For Gemini, use `gemini-web`, `--kind gemini-web`, `~/.config/tincan/gemini-web.json` and `--site gemini` (plist `com.agenttincan.web.gemini.plist`), and first grant Gemini on the extension's options page (see [Gemini](#gemini)).

`tincan web install` writes the launchd agent (a systemd user unit on Linux, `tincan-chatgpt-web.service`) and prints the command that starts it. It never starts anything itself. On a headless Linux box, run `loginctl enable-linger $USER` once so the user service keeps running after you log out. The service logs to `~/Library/Logs/tincan-chatgpt-web.log`. It refuses to start unless the relay confirms it is `chatgpt-web` (or the name given with `--name`), so a config for another agent can never claim that agent's requests. Flags for running by hand: `--site`, `--name`, `--config`, `--allowlist`, `--state`.

Set its wake method to `wait` in the relay's `wake.json`; the service long-polls. `tincan onboard` prints these steps for the `chatgpt-web`, `claude-web`, `grok-web` and `gemini-web` kinds. A relay older than this release does not know the `grok-web` or `gemini-web` kind: upgrade the relay first, or invite without `--kind` and pass `tincan onboard --kind <name>=<kind>`.

### Extension updates

The ChatGPT and Claude send operations need extension version 0.3.0 or later (0.2.0 waited for the answer on the page); Grok and Gemini need 0.4.0, which adds the options page that grants them. Load the extension unpacked once from `chrome://extensions` (the unzipped `tincan-history-extension.zip`, or `extension/` in a repo checkout). After that, updates need no Reload click: run `tincan history install --extension-dir <the folder you loaded>` (or run it from a repo checkout), and whenever the extension connects (and every 10 minutes while it stays connected), the native host compares its version and file hashes with the files on disk and, if they differ, sends the fixed `extension.reload` operation. The extension hashes its files once when its worker starts, so the hashes describe the code Chrome loaded even after the files on disk change. On `extension.reload` it calls `chrome.runtime.reload()` and Chrome re-reads the files, but not while a send is typing in a tab or a finished send's tab is waiting to be closed (a reload would lose track of those tabs): it checks again every 5 seconds for up to 5 minutes. At that cap it closes the finished sends' tabs and reloads anyway; a send still typing then is abandoned, its tab stays open, and that request fails when its wait times out. The host asks at most once per 10 minutes for the same files when the extension connects, and the 10 minute re-check never asks again for files it already asked about, so a copy loaded from somewhere else cannot cause a reload loop; only the files changing again, or a reconnect, asks again. A store install is never reloaded this way.

## Troubleshooting

- "Declined: X is not on the chatgpt-web allowlist": add X to `~/.config/tincan/chatgpt-web-allow.txt` if you want it to act as you in ChatGPT.
- "Declined: this request came through X": an allowed agent passed on X's request. Every agent in the chain must be allowed.
- "source unavailable: chatgpt: not logged in to chatgpt.com in Chrome" (or claude.ai, grok.com): log in in Chrome. The extension checks the session before it opens a tab, so a logged-out browser never sends anonymously.
- "source unavailable: ...: the Tincan Chrome extension is not connected": install or enable the extension, then run `tincan history install`.
- "the Tincan Chrome extension has no access to chatgpt.com; grant it on the extension's options page": Chrome has not granted the extension that site (it was withheld in the extension's site access settings, or the site is one that must be granted first). Open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant. While the extension is connected and reports such a site ungranted, `tincan web serve` logs this once and waits, checking again every minute, and starts serving as soon as the grant appears (no restart needed); with no extension connected it starts and answers with the not-connected reply until Chrome is up. Withholding only ChatGPT's file host (`*.oaiusercontent.com`) does not count as ungranted: conversations still list and read, and only images stored on that host fail to download.
- "chatgpt.com showed an anti-bot check": the site answered with a Cloudflare challenge, an anti-bot refusal (grok.com's 403 "Request rejected by anti-bot rules") or Google's "unusual traffic" page instead of its API, or the send tab opened on a challenge page. Open the site in Chrome, complete the check, then ask again.
- "the Tincan Chrome extension has no access to grok.com": grant Grok on the extension's options page (above).
- "source unavailable: ...: the extension rejected the request (unknown operation; the loaded extension is older than this tincan ...)": the loaded extension predates the send operations. Reload it once from `chrome://extensions`; later updates reload themselves.
- "no message box on the chatgpt.com page (the page may have changed)": the site changed its page, or showed an interstitial (a consent or upgrade dialog). Open the site in Chrome, dismiss anything in the way, and ask again. If it persists, the selectors in `extension/send.js` need an update.
- "the message could not be sent on chatgpt.com": the text did not land in the message box, the send button stayed disabled, or the page ignored the click (for example a usage limit). Open the site and look.
- "ChatGPT did not finish answering in time": no finished answer before the request timeout. The reply names the conversation when the message was sent; look in it and ask again with `conversation: <id>`. If the answer is still coming, wait for it to finish first, or the send is refused (see below).
- "the conversation is still answering an earlier message": the conversation still shows a stop button or a streaming answer, so the send was refused. Wait for the answer on the site to finish and ask again, or use `new chat`.
- "... The message was sent to ChatGPT (conversation X), but the reply could not be read": the send worked but the detail read failed for good (logged out, API changed). The answer is in the conversation on the site.
- "No ChatGPT conversation with id X was found": the id is wrong or the conversation was deleted. Use `new chat`.
- Background tabs: Chrome throttles hidden tabs. Nothing waits on the page after the send; the answer is read through the site's API.
- No reply at all: check the service is loaded (`launchctl print gui/$(id -u)/com.agenttincan.web.chatgpt`) and read its log. A "403" there means the config is not joined.

### Selectors

All page selectors live in one table, `SELECTORS` in `extension/send.js`, each with fallbacks tried in order (grok.com in the second table):

| Role | chatgpt.com | claude.ai | gemini.google.com |
| --- | --- | --- | --- |
| message box | `#prompt-textarea`, `div[contenteditable="true"][id="prompt-textarea"]`, `textarea[data-id="root"]`, `form div[contenteditable="true"]` | `div[contenteditable="true"].ProseMirror`, `fieldset div[contenteditable="true"]`, `[contenteditable="true"][aria-label*="prompt" i]`, `div[contenteditable="true"]` | `div.ql-editor[aria-label="Enter a prompt for Gemini"]`, `rich-textarea div.ql-editor[contenteditable="true"]`, `div.ql-editor[contenteditable="true"]` |
| send button | `[data-testid="send-button"]`, `#composer-submit-button`, `button[aria-label*="Send"]` (else Enter) | `button[aria-label="Send message"]`, `button[aria-label*="Send"]`, `fieldset button[type="submit"]` (else Enter) | `button[aria-label*="Send" i]`, `button.send-button` (appears once there is text; else Enter) |
| answering (refuses a send while present; confirms a send after the click) | `[data-testid="stop-button"]`, `button[aria-label*="Stop"]`, `.result-streaming` | `button[aria-label="Stop response"]`, `button[aria-label*="Stop"]`, `[data-is-streaming="true"]` | `button[aria-label*="Stop" i]` |
| assistant messages | `[data-message-author-role="assistant"]` | `[data-is-streaming]`, `.font-claude-response`, `.font-claude-message`, `[data-testid="assistant-message"]` | `model-response`, `message-content` |
| user messages | `[data-message-author-role="user"]` | `[data-testid="user-message"]` | `user-query` |
| logged out | `[data-testid="login-button"]`, `a[href*="/auth/login"]`, paths `/auth/login`, `/log-in` | `a[href="/login"]`, `input[type="email"]`, paths `/login`, `/logout` | `a[href*="accounts.google.com/ServiceLogin"]`, `a[href*="accounts.google.com/v3/signin"]` |

On every site, a send tab the site sends to another host is not typed into: Google's `/sorry/` page is `blocked`, any other host (a sign-in page) is `not_logged_in`. When that happens after the send button was clicked, the message may already be in the conversation, so the failure reply says so and asks you to check the conversation before sending it again.

| Role | grok.com |
| --- | --- |
| message box | `div[contenteditable="true"][aria-label="Ask Grok anything"]`, `form div.ProseMirror[contenteditable="true"]`, `div[contenteditable="true"].ProseMirror`, `textarea[aria-label*="Ask Grok"]` |
| send button (appears only once there is text) | `form button[type="submit"][aria-label="Submit"]`, `button[aria-label="Submit"]`, `form button[type="submit"]` (else Enter) |
| answering | `button[aria-label="Stop model response"]`, `button[aria-label*="Stop"]`, `[data-streaming="true"]` |
| assistant messages | `div[id^="response-"].items-start` |
| user messages | `div[id^="response-"].items-end` |
| logged out | `a[href^="/sign-in"]`, `a[href*="accounts.x.ai/sign-in"]`, `a[href*="/sign-up"]`, paths `/sign-in`, `/sign-up` |
| anti-bot page (fails `blocked` before anything is typed) | `#challenge-form`, `iframe[src*="challenges.cloudflare.com"]`, `#cf-challenge-running`, a "Just a moment..." title |

The text is entered by typing (`document.execCommand('insertText')`), then a paste event, then setting it directly, and checked after each attempt. Before each click the page is probed again, and an existing conversation's address is checked again. The send counts as taken when, compared with that probe, a new chat's address gains a conversation id, or a new user or assistant message or an answering marker appears. The page is never used to decide that an answer is finished.

### When an answer is finished

The web agent decides from the conversation detail, never from the page:

First it finds this request's own user message: the first user (claude.ai: human) message on the current branch after the one that was last before the send (read just before sending into an existing conversation), whose text is the text sent (ignoring whitespace and the markdown marks ``#*`>_-``, the same comparison the extension uses to check the message box), and that is not dated more than 2 minutes before the extension's `submitted_at`. Once seen, that message is fixed for the rest of the wait. So neither a finished answer to an earlier message, even with the same text, nor the answer to a message sent later in the same conversation (you typing there meanwhile) is taken for this one.

The answer is the last message after that user message and before the next user message on the branch, if any:

- ChatGPT: an assistant message to everyone, with status `finished_successfully`, `finish_details`, or `end_turn: true` (and `end_turn` not `false`).
- claude.ai: an assistant message with a `stop_reason`, or else whose text is the same on 4 reads in a row spanning at least 10 seconds, so a pause mid-answer is not taken for the end.
- Grok: an assistant response with `partial: false` while the conversation's `inflightResponses` is empty. grok.com marks this explicitly, so there is no text-stability wait. The current branch is walked back through `parentResponseId` from the newest response, so a regenerated answer replaces the draft it regenerated.
- Gemini: its read has no finished marker, so the chosen answer counts as finished once its text (and image count) is the same on 4 reads in a row spanning at least 45 seconds. Gemini can hold its text still for a long while as it thinks or searches, so the window is longer than claude.ai's; an answer still changing when the 8 minutes run out ends with the "did not finish answering in time" reply.
If a later user message follows this request's message with nothing between them, no answer will come, and the request fails saying another message was sent in the conversation first. If this request's answer is still being written when a later message appears, the agent keeps waiting for it.

## Gemini

`gemini-web` (`tincan web serve --site gemini`) works like the other two, with these differences.

Account risk. Google's terms do not allow automated access to Gemini. The agent acts as you, on your own account, at a human pace (one request at a time, reads 5 to 20 seconds apart, a cooldown after any rate limit or anti-bot page), but that does not make it permitted, and Google enforcement is not limited to Gemini: it can reach your whole Google account (Gmail, Drive, everything else signed in with it). Decide whether that risk is acceptable before you run it.

What Gemini can read. Gemini answers can draw on the Google apps connected to your account, such as Gmail, Drive and Calendar. Anyone who can ask `gemini-web` can therefore read that data through it ("summarize my latest emails"), not just chat with Gemini. With no allowlist file every agent joined to your relay may ask, so for `gemini-web` write `~/.config/tincan/gemini-web-allow.txt` listing only the agents that should, or disconnect those apps in Gemini's settings.

Grant first. Gemini is an optional site in the extension: an upgrade asks Chrome for nothing new. Open `chrome://extensions` > Agent Tincan History > Details > Extension options and click Grant next to Gemini. The grant covers `gemini.google.com` and `lh3.googleusercontent.com`, where Gemini's images are served. Until then every Gemini operation fails `permission_missing` without a tab or a request, `tincan web serve --site gemini` logs it and waits while the extension is connected, checking again every minute and serving as soon as the grant appears, and requests get a reply naming the options page. Withholding only the image host leaves list, read and send working; only image downloads fail.

Reads. Gemini has no JSON API of its own, so the extension's service worker fetches the app page (`/app`) for its per-session values (`SNlM0e`, `cfb2h`, `FdrFJe`, kept in the worker's memory for 10 minutes and never returned), then calls the app's `batchexecute` endpoint with two fixed rpcids: `MaZiqc` lists conversations 13 at a time, following each page's token, and `hNvQHb` reads a conversation's latest 10 turns. The extension hands back the decoded payloads as data; Go reads their positions defensively, and any shape it does not recognize is reported as "gemini.google.com changed its API", never a crash. A stale token (HTTP 400 or 401) fetches the app page again once.

Conversation ids. The canonical id is the hex in the address, `https://gemini.google.com/app/<id>` (16 hex digits). `batchexecute` calls it `c_<id>`; that form exists only inside the extension. `conversation:` accepts `/app/<id>`, `/u/<n>/app/<id>`, `/gem/<name>/<id>` and `c_<id>`, and every store (the per-asker state, the used list, the send journal, the reply footer `Gemini conversation: <id>`) holds the hex. The agent uses the first Google account signed in to Chrome (`/app`, not `/u/1/`); a conversation of another signed-in account is not found.

Images. Gemini's generated images are rendered from `lh3.googleusercontent.com`. The fixed `gemini.file` operation never takes a URL: it gets the conversation id and `<response id>-<n>`, reads the conversation again, and takes the n-th image URL of that response, only on that host. It then tries, in order: a fetch inside the tab the send left open (the isolated world, with the page's cookies, the same request the page makes), then a fetch from the service worker with the image host's grant. It never draws the page's `<img>` onto a canvas: a cross-origin image taints it. If both fail, the reply is the text plus a note that Gemini's reply had images that could not be attached; open the conversation to see them. Which of the two attempts works against the live site has not been confirmed yet. Gemini's text stands in for a generated image with a `googleusercontent.com/..._content/<n>` link; those are dropped from the reply text. History reads fetch images the worker way only (there is no send tab).

Not in v1: Deep Research (it needs a mode switch and a plan confirmation the plain composer send does not do), and choosing a model or a Gem for a new chat (a `conversation:` in a Gem's chat continues it).

### Retries and duplicate sends

The relay requeues a claimed request whose reply never arrives (after its 30-minute claim lease), for example when the agent crashed or the reply failed to reach the relay. A send is not repeated for that: right after the extension confirms a send, the agent records the request id, conversation id, the id of the message it sent (once seen) and `submitted_at` in `~/.config/tincan/<agent>-journal.json` (mode 0600; ids and times only, never messages or answers), and marks the entry answered once it replies. A request found there is not sent again; the agent reads the answer from the journaled conversation (waiting for it if needed) and replies. Entries are dropped after 90 minutes.

## Ask both web agents

Run `tincan ask chatgpt-web,claude-web "Compare these options"`, or call MCP
`ask` with `{"to":"chatgpt-web","also":["claude-web"],"message":"Compare these options"}`.
Both receive the same question, with answers labeled by teammate. A partial
result includes a group id: pass it to `get_reply` as `request_id` or run
`tincan get <group-id>` to gather later answers. Each web agent still applies
its own allowlist. Attachments are uploaded once per target.
