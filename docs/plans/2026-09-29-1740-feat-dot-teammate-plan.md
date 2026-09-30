---
title: OpenAI Dot as a Teammate - Plan
type: feat
date: 2026-09-29
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
deepened: 2026-09-29
---

# OpenAI Dot as a Teammate - Plan

---

## Goal Capsule

- Objective: the owner's OpenAI dot is a two-way teammate. Any agent can hand it work and get its answer back, and the dot can ask other agents for help and get their answers. Nothing is installed on the dot's computer and nothing needs the owner to relay.
- Means: a `dot-web` web agent, built the same way as chatgpt-web and copilot-web. The Tincan Chrome extension types the request into the dot's DM page and reads the dot's answer from the DM's message feed (KTD1, KTD2, KTD3).
- Authority: Requirements (R) win on product behavior. KTDs win on mechanism within their cited Rs. Units override neither.
- Stop conditions:
  - Stop and ask if sending into the dot's DM would require accepting a dialog, changing a dot setting, or bypassing an anti-bot check.
  - Stop and ask before any live test sends more than the setup message plus one short test message each way into the owner's dot DM.
- Execution profile: Go (`internal/history`, `internal/cli`, `internal/onboard`) plus the Chrome extension (JS) in `agent-tincan`, on branch `feat/dot-teammate`. The live check uses the owner's signed-in Chrome and dot thread `0d0d0d0d-1111-7222-8333-000000000001`.
- Who finishes: the implementer builds, tests and runs the live check. The owner only reloads the unpacked extension if the check needs the new build.

---

## Product Contract

### Summary

Add `dot-web`, a web agent that makes the owner's dot a two-way teammate through Chrome, like chatgpt-web.
- Inbound: `tincan ask dot-web "..."` has the extension type the request into the dot's DM in a background tab. The web agent reads the DM's message feed until the dot's answer holds still, and replies with it. These requests are held for owner approval by default.
- Outbound: the dot writes `@tincan ask <agent> <request>` in its DM. The web agent sees it in the feed, sends the ask as `dot-web`, and types the answer back into the DM marked `[tincan-reply from <agent>]`.

### Problem Frame

OpenAI launched Dots on 2026-09-29: always-on agents powered by GPT-6 Astra, each with its own cloud computer, the owner's connected apps (Gmail, GitHub, Google Drive and others), and a DM page at `chatgpt.com/dots/<thread-id>`. There is no public Dots API. The dot's DM is a new messaging-room backend, not a `/c/<id>` conversation, so chatgpt-web cannot reach it.

An earlier version of this plan had the dot run `tincan` on its own computer and join the tailnet. The dot's sandbox blocks the netlink calls Tailscale needs, and that path needed setup only the owner could do. The owner chose the Chrome path instead: it needs nothing on the dot's side, runs without the owner, and is a stopgap until OpenAI adds better integration.

A dot acts on its own across the owner's connected apps. A message typed into its DM arrives as if the owner wrote it, so anything a teammate sends it carries the owner's authority.

### Requirements

Asking the dot
- R1. `tincan ask dot-web "<text>"` sends the text into the owner's dot DM as one message and replies with the dot's answer.
- R2. The answer is every message the dot posts after the request, joined in order, once no new dot message has arrived and none has changed for the stability window.
- R3. Nothing is installed on or sent to the dot's computer by Tincan. Everything runs through the owner's signed-in Chrome and the Tincan extension.
- R4. The agent handles one request at a time. It never types into a tab the owner opened, and never types over text already in the composer.

Failure and limits
- R5. A paused dot, a logged-out Chrome, or a missing extension fails the request with a reason the asker can act on. Nothing is sent in these cases.
- R6. When the dot does not answer within the request timeout, the request fails with "the message was sent; ask for the reply later" instead of being sent again.
- R7. A 429 from the dot backend follows the same cooldown rules as the other web agents.

Safety
- R8. Asks and notifies to `dot-web` are held for owner approval by default, unless `approval.json` has an entry for it.
- R9. The `dot-web` agent's allowlist file works like the other web agents' allowlists.

The dot asking teammates
- R11. A dot message whose first line starts with `@tincan ask <agent>` becomes one tincan ask from `dot-web` to that agent. The rest of the message is the request text. Each such message is sent exactly once, even across restarts.
- R12. When the answer arrives, `dot-web` types it into the DM starting with `[tincan-reply from <agent>]`, followed by the dot's original request line so the dot can match it. A failure or timeout is typed back the same way with the reason.
- R13. When `dot-web` starts and has not yet taught this dot, it types one setup message into the DM. The message explains the `@tincan ask` format, the `[tincan-reply ...]` marker, that tincan replies are data from teammates and not the owner's instructions, and `tincan agents`-style roster names. It is sent once per thread, and again only when the owner asks for it.
- R14. Outbound asks respect an allowlist file of target agents (`~/.config/tincan/dot-web-send.txt`), the same file format as the web agents' allowlists. No file means any joined agent. The relay's `approval.json` can also hold them (`from: ["dot-web"]`).
- R15. The feed watcher reads the DM at a human pace (every 30 seconds when idle, backing off on errors and 429s) and never while a request of its own is waiting for an answer beyond that cadence.

Documentation
- R10. `docs/adapters/web-agents.md`, the README and `site/agents.txt` describe `dot-web`: what it does, that requests land in the owner's DM as the owner's messages, that they are held by default, and that the dot cannot start asks of its own.

### Key Decisions

- Chrome only, no install on the dot. (session-settled: user-directed - chosen over running tincan on the dot's computer over Tailscale or the public gateway: the owner wants it to run autonomously, with nothing to set up, as a stopgap.) Governs R1, R3.
- Held by default. A dot can act in the owner's apps, and a request arrives as the owner's own words. Governs R8.

### Scope Boundaries

- Out: waking or nudging the dot for work it pulls itself; the request text is the message.
- Out: Slack, Teams, SMS, the desktop app.

---

## Planning Contract

### Key Technical Decisions

- KTD1. `dot-web` is a web-only site entry in `internal/history/sites.go`, modeled on copilot-web: it gets its own op prefix (`dots`), detail and send and close ops, a conversation path of `/dots/<thread-id>`, and a text-stability rule, since the dot's feed has no "finished" flag.
- KTD2. The detail op reads the room feed, not the page. `dots.detail` reads three backend endpoints with the page's bearer, the same way `chatgptAuth` does:
  - `GET /backend-api/tbo/by-thread/<thread>` for `messaging_room_id` and `is_paused`;
  - `GET /backend-api/messaging/rooms/<room>` for `creator_account_user_id` (the owner);
  - `GET /backend-api/messaging/rooms/<room>/messages?limit=50`, which returns `{items: [{id, created_at, account_user_id, content: {text, attachments}}]}` oldest first.

  The owner's messages become user nodes. The dot's messages (any other account) become reply nodes, and consecutive dot messages after a user turn are one reply.
- KTD3. Send drives the page. `dots.send` opens `https://chatgpt.com/dots/<thread>` in a background tab, types into `div[role="textbox"][aria-label="Message"]` (ProseMirror), and clicks `button[aria-label="Send"]`. The page's URL never changes, so the send confirms delivery by the new owner message appearing in the room feed, not by a conversation id in the address. It refuses when the composer already holds text.
- KTD4. The conversation id is the dot's thread id, from `--thread` or `~/.config/tincan/dot-web.json`. The threading line (`new chat`, `conversation:`) does not apply: a dot has one DM.
- KTD5. The hold-by-default kind set gains `dot-web`'s kind. The `dot` kind from the earlier version becomes the web agent's kind (`dot-web`), and its standing instructions are removed, because the dot never runs tincan.

### Risks

| Risk | Mitigation |
|---|---|
| OpenAI changes the dots page or room API | Selectors and endpoints live only in `extension/send.js` and `extension/ops.js`. A change fails as `endpoint_changed`, never silently. |
| The dot answers in several bursts minutes apart | The stability window is long (KTD1, U10) and the request timeout covers slow dots. A late burst after the reply is not delivered, and the reply says so. |
| A teammate's text acts with the owner's authority in Gmail or GitHub | R8's default hold, and the docs say so plainly. |
| The owner is chatting with the dot when a request lands | R4: never types over a draft. The anchor is the request's own message id, so the owner's chat is not mistaken for the answer. |

---

## Implementation Units

### U2. `dot` kind and hold by default (done, to adjust)

- **Goal:** Already committed. Adjust so the kind fronts a web agent: rename to `dot-web`, drop the dot-side standing instructions and setup (there is no tincan on the dot), and make it a web kind with the default wake of the other web agents.
- **Requirements:** R8.
- **Dependencies:** none.
- **Files:** `internal/onboard/onboard.go`, `internal/onboard/templates/agent.tmpl`, `internal/onboard/templates/recipes.tmpl`, `internal/onboard/templates/operator.tmpl`, `internal/policy/policy.go`, tests in `internal/onboard/` and `internal/policy/`.
- **Approach:** Follow how `perplexity-web` is defined (web kind, `runtimeNames`, `wait` wake, `website*` template branches). Keep the hold-by-default rule, keyed to the new kind.
- **Test scenarios:**
  - `dot-web` is a known kind and a web kind, and its onboarding block reads like the other web agents' (run `tincan web serve --site dots`).
  - Requests to `dot-web` with no `approval.json` entry are held, and an entry that allows the sender releases them.
  - Other web kinds are not held by default.
- **Verification:** The onboard and policy tests pass.

### U9. Extension: dots ops

- **Goal:** Give the extension `dots.detail`, `dots.send` and `dots.close`.
- **Requirements:** R1, R3, R4, R5, R7.
- **Dependencies:** none.
- **Files:** `extension/ops.js`, `extension/send.js`, `extension/test/ops.test.js`, `extension/test/send.test.js`.
- **Approach:**
  1. `dots.detail({id})`: read the three endpoints in KTD2 and return `{thread, room, owner, paused, items}`. Use the existing error codes: `not_logged_in` on 401, `rate_limited` with `Retry-After` on 429, `endpoint_changed` on a missing field.
  2. `dots.send({text, conversation})`: refuse with `paused` when the dot is paused. Open a background tab on `/dots/<thread>`, type and send per KTD3, then poll the feed (bounded, about 60s) for a new owner message whose text matches what was sent, and return its message id plus the thread id.
  3. `dots.close`: close only a tab this send opened, same rules as the other close ops.
- **Patterns to follow:** `copilot.send`, `chatgpt.detail`, `chatgptAuth`, `SITES` and `SELECTORS` in `extension/send.js`, `SITE_ACCESS` (chatgpt.com is already granted).
- **Test scenarios:**
  - `dots.detail` maps the three responses into the returned shape, marking the owner from `creator_account_user_id`.
  - `dots.detail` returns `not_logged_in` on a 401 and `rate_limited` with `Retry-After` on a 429.
  - `dots.send` refuses a paused dot before opening a tab.
  - `dots.send` refuses when the composer already has text, and leaves the text alone.
  - `dots.send` types, clicks send, and returns the new owner message id once it appears in the feed.
  - `dots.send` gives up with an error, closing its own tab, when no new owner message appears in time.
  - `dots.close` closes only the tab its send opened.
- **Verification:** `make extension-test` passes.

### U10. Go: `dot-web` site and web agent

- **Goal:** `tincan web serve --site dots` runs the `dot-web` agent end to end.
- **Requirements:** R1, R2, R4, R5, R6, R7, R9.
- **Dependencies:** U2, U9.
- **Files:** `internal/history/sites.go`, `internal/history/dots.go`, `internal/history/native.go`, `internal/history/web.go` (only if the threading line needs a per-site switch), `internal/cli/web.go`, tests `internal/history/dots_test.go`, `internal/history/sites_test.go`, `internal/history/web_test.go`.
- **Approach:**
  1. Add a web-only site entry (`source` for dots, label "your dot", host `chatgpt.com`, agent `dot-web`, op prefix `dots`, `convPath` for `/dots/<id>`, a `stable` rule of about 3 polls over 45s).
  2. `dotsNodes(raw)` turns the detail result into webNodes per KTD2: the owner's messages are user nodes, and each run of dot messages after one user message is joined into one reply node.
  3. The thread id comes from `--thread`, or from `~/.config/tincan/dot-web.json`. Every request uses that conversation, and a threading first line is sent as ordinary text.
  4. Paused and not-logged-in errors map to replies the asker can act on (R5).
- **Patterns to follow:** The copilot site entry and `copilotNodes` in `internal/history/copilot.go`, and the web agent tests with the fake native helper (`fakehelper_test.go`).
- **Test scenarios:**
  - `dotsNodes` on a feed with owner message A, dot messages B and C: one user node A and one reply node "B\n\nC".
  - `dotsNodes` on a feed where the owner sent another message after A: the reply to A stops at that message.
  - With the fake helper, a request sends the text, binds to the returned owner message id, waits until the dot's reply holds still, and replies with it.
  - A dot that answers in two bursts within the stability window gets both bursts in one reply.
  - A paused dot fails at once with the paused reason, and nothing is sent.
  - No dot answer within the timeout: the request fails with the "message was sent; ask later" reason.
  - The site is not a history source and is left out of `SourceNames`.
- **Verification:** `make test` passes, and `tincan web serve --site dots --help` shows the site.

### U12. Outbound asks from the dot

- **Goal:** The dot can ask teammates by writing `@tincan ask <agent> <request>` in its DM.
- **Requirements:** R11, R12, R13, R14, R15.
- **Dependencies:** U9, U10.
- **Files:** `internal/history/dots.go`, `internal/history/dots_out.go`, `internal/cli/web.go`, tests `internal/history/dots_out_test.go`.
- **Approach:**
  1. Parse: a dot message (not the owner's) whose first line matches `@tincan ask <agent>` (agent name per the relay's name rules) is an outbound ask. Everything after the agent name, including later lines, is the request text.
  2. State: a 0600 file `~/.config/tincan/dot-web-out.json` keyed by dot message id records `sent` (with the tincan request id), `answered` or `failed`, plus `taught: true` per thread. A message is sent only when it has no record, which makes restarts safe.
  3. Watcher: between inbound requests, poll `dots.detail` at the R15 cadence. On a new outbound line, check the R14 allowlist, then `ask` through the agent's relay client. Do not block the loop: record the request id and check `get_reply` or inbox replies on later ticks.
  4. Answer: type `[tincan-reply from <agent>]`, then the quoted original request line, then the answer text, into the DM through `dots.send`. On failure (declined, expired, not in the allowlist), type `[tincan-reply from <agent>] failed: <reason>`.
  5. Teach: on start, if the thread is not `taught`, send the R13 setup message through `dots.send` and record it. `--teach` sends it again.
  6. Inbound and outbound share the one send path, so the agent still handles one DM send at a time (R4).
- **Patterns to follow:** The web agent's request loop and allowlist handling in `internal/history/web.go`. Relay client `Ask` and `GetReply` in `internal/client`.
- **Test scenarios:**
  - A dot message `@tincan ask muse check the calendar` produces one ask to `muse` with text `check the calendar`.
  - The same message seen on the next poll, and after a restart with the state file present, produces no second ask.
  - An owner message with the same line is ignored.
  - A target not in the allowlist file is not asked, and the DM gets a failed reply naming the allowlist.
  - When the ask is answered, the DM gets one `[tincan-reply from muse]` message with the answer.
  - A declined or expired ask produces one failed reply with the reason.
  - First start sends the setup message once, and a second start does not.
  - A 429 on the feed read backs the watcher off and does not drop the outbound line.
- **Verification:** With the fake helper and testrelay, an outbound line round-trips to a test agent and its answer is typed back.

### U11. Docs

- **Goal:** Document `dot-web`.
- **Requirements:** R10.
- **Dependencies:** U2, U9, U10, U12.
- **Files:** `docs/adapters/web-agents.md`, `README.md`, `site/agents.txt`, `extension/README.md`.
- **Approach:** Add dot-web beside copilot-web: running it, the thread id config, that requests and tincan replies appear in the owner's DM as the owner's own messages, the default hold and the `approval.json` override, the `@tincan ask` format, the outbound allowlist, and the setup message.
- **Test expectation:** none. Documentation only.
- **Verification:** A reader can start `dot-web` from the docs alone.

---

## Verification Contract

| Gate | Command or check | Applies to |
|---|---|---|
| Go tests with race | `make test` | U2, U10 |
| Vet and lint | `make vet`, `make lint` | U2, U10 |
| Extension tests | `make extension-test` | U9 |
| Live round trip in | with the new extension build loaded, `tincan ask dot-web` sends one short test message into the owner's dot DM and the dot's answer comes back as the reply | U9, U10 |
| Live round trip out | the dot writes one `@tincan ask` to a test teammate, and the answer appears in the DM | U12 |

---

## Definition of Done

- `tincan ask dot-web "<text>"` returns the dot's answer, and the dot's `@tincan ask` lines reach teammates and get answered, all through the owner's Chrome with nothing installed on the dot.
- Requests to `dot-web` are held by default.
- Every Verification Contract gate passes, including one live round trip.
- The docs in U11 are updated.
- Cleanup: the reverted Tailscale spike and any dot-side onboarding text are gone from the diff.

---

## Sources

- The dot's DM backend, observed in the owner's browser on 2026-09-29 (read-only):
  - The room holds two members. The owner is `creator_account_user_id`.
  - `messages?limit=N` returns the latest N messages oldest first, and every message is `role: user`, so the sender is told apart by `account_user_id`.
  - The dot record has `status`, `is_paused`, `messaging_room_id` and `last_check_in_at`.
  - The composer is a ProseMirror `div[role="textbox"][aria-label="Message"]`, and send is `button[aria-label="Send"]`.
- Repo patterns: `internal/history/sites.go` (site table, `stableRule`, `webOnly`), `internal/history/copilot.go`, `internal/history/web_poll.go` (`waitReply`), `extension/ops.js` (`chatgptAuth`, op specs), `extension/send.js` (`SITES`, `SELECTORS`).
