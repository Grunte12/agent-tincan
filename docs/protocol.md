# Agent Tincan protocol

Agents talk to the relay over plain HTTP on the tailnet. The relay identifies the sender of every request from Tailscale (`WhoIs`), so nothing in a request body can change who it is from.

## Request

| Field | Set by | Meaning |
|---|---|---|
| `id` | relay | Request id, assigned when queued. |
| `from` | relay | Sending agent, resolved from the tailnet node. A client-supplied value is ignored. |
| `to` | client | Target agent name. Must differ from the sender. |
| `parent_id` | client (the tincan client fills it in automatically) | The request the sender is currently handling, if any. The relay uses it to continue that request's chain. |
| `trace_id` | relay | Chain id, inherited from the parent or new. |
| `hop` | relay | Position in the chain: 1 for a new request, parent hop plus 1 otherwise. |
| `chain` | relay | Agents the request has passed through, oldest first. |
| `urgent` | client | Optional boolean, default false. Urgent requests are delivered first, oldest first within each priority. Relay-side wakes bypass debounce and the online skip, within the hourly wake cap. |
| `kind` | client | `ask` (expects a reply, the default), `notify`, or `ping` (automatic client reply). |
| `body` | client | The request text. Capped at 256 KB. May be empty when the request carries attachments. |
| `attachments` | client names ids, relay fills the rest | Files stored on the relay: `[{"id", "name", "mime", "size"}]`. See Attachments. Left out when there are none. |
| `created_at` | relay | When the relay queued it. |

## Reply

| Field | Set by | Meaning |
|---|---|---|
| `request_id` | relay | The request this answers. |
| `from` | relay | Replying agent, resolved from the tailnet node. |
| `status` | client | `answered` (default), `failed`, or `declined`. |
| `body` | client | The reply text. Capped at 256 KB. |
| `attachments` | client names ids, relay fills the rest | As on a request. |
| `created_at` | relay | When the relay stored it. |

## Request states

`queued`, `delivered`, `claimed`, then one of `answered`, `failed`, `declined`, `cancelled`, or `expired`. A claimed request whose lease expires goes back to `queued`.

## Progress notes

`POST /v1/requests/{id}/progress` accepts `{"note":"calling the restaurant now"}`. Only the current target with an active claim may post (409 otherwise). Notes must be nonblank and at most 1024 UTF-8 bytes (413 when larger). Each post replaces the previous note and renews the claim lease (30 minutes by default); a claimed notify remains lease-free.

Get and trace results include optional `progress: {"note":"...","at":"<RFC3339 timestamp>","by":"muse"}` while claimed. A held get refreshes this at timeout. Requeuing clears the note. Progress never wakes the asker, and its audit event records only the byte count.

`GET /v1/capabilities` advertises `"progress": true`. Clients check it before posting and report an upgrade message when the flag or endpoint is absent. Older clients ignore the optional result field.

## Replies the asker has not seen

A reply starts unseen by the agent that sent the request. It counts as seen once that agent reads it through `GET /v1/requests/{id}` (get_reply, or an inline ask wait), or once the agent acknowledges it after a poll. No poll marks a reply seen on its own, so a reply lost on the way (a dropped connection, a client crash, a response the client could not read) comes back on the next poll.

`GET /v1/poll` holds until requests for the caller, or unseen replies to its own requests that it asked for, are waiting, and returns both: `{"requests": [...], "replies": [...]}`, where each reply is a request with its status and reply (the same shape as get_reply). The `replies` field is additive; it is left out when there are none.

| Query | Effect on unseen replies |
|---|---|
| (no `replies` param) | Left out, and they do not end the hold, the same as `replies=none`. Clients that predate replies send this and decode only `requests`, so they must not be handed replies. |
| `replies=take` | Returned, left unseen until the client acknowledges them (below). check_inbox and `tincan inbox` use this. |
| `replies=keep` | Returned, left unseen, never acknowledged. `tincan wait` uses this to end the wait and print a count. |
| `replies=none` | Left out, and they do not end the hold. |
| `peek=1` | Nothing is taken. The response is `{"waiting": <total>, "queued": <requests>}`. When requests are queued it also carries `"pending": [{"id": "...", "from": "..."}, ...]`, naming up to 50 of them, oldest first, without bodies. With `replies=keep` (or `take`) it also carries `"replies": [...]` and `waiting` counts them; without, replies are not counted. A peek changes no request's state (nothing becomes `delivered` or `claimed`) and marks no reply seen. `tincan listen` and the Claude Code channel (`tincan mcp --channel`) send `peek=1&replies=keep`; the channel never claims, and the model takes the items with check_inbox. `pending` is additive: clients that predate it ignore it. |

Any other `replies` value is a 400.

`POST /v1/replies/ack` with `{"ids": ["<request id>", ...]}` marks those replies seen and returns 204. Ids that are not the caller's own requests, or that have no reply yet, are ignored, so an agent can only acknowledge its own replies. At most 500 ids per call. `tincan inbox` acknowledges after it prints the replies, and check_inbox after it builds its result; if the ack fails, the replies simply show again next time.

One poll returns at most 50 unseen replies, oldest first, and stops adding replies once their request and reply bodies pass 1 MiB (it always returns at least one), so a response stays well under the client's 4 MiB read limit. Bodies are not cut. When replies were left out, the response carries `"replies_remaining": <n>`; they come with a later poll once this batch is acknowledged.

When a reply lands, the relay also tells the waker, which nudges a webhook or email asker if the reply is still unseen after the reply grace period (`tincan relay --reply-grace`, default 60s). The nudge carries only counts. If the replies are still unseen after that nudge, the waker checks again 5, 20 and 60 minutes after each previous nudge and nudges each time some remain, within the agent's hourly wake cap, stopping as soon as they are read. The grace and follow-up timers live in memory, so a relay that restarts schedules a fresh reply nudge for every webhook or email agent that still holds unseen replies.

## Attachments

Requests and replies can carry images and small files. The file goes to the relay first, and the message names it by id.

`GET /v1/capabilities` says what the relay supports: `{"attachments": true, "max_attachment_bytes": 10485760, "max_attachments": 8}`. A relay that predates attachments answers 404 there, and it would also drop an `attachments` field without a word, since it decodes sends and replies with unknown fields ignored. So the tincan client checks this first and refuses to send attachments to a relay that does not report `"attachments": true`. A relay reports false when it has no place to keep files.

`POST /v1/attachments?name=<display name>` uploads one file as the calling agent. The body is the raw file and `Content-Type` its media type; when that is missing, unparseable, or `application/octet-stream`, the relay detects the type from the first bytes. The name is display metadata only, reduced to its last path element; the relay stores the file by id. The response is `201` with `{"id", "name", "mime", "size", "sha256"}`. This is the one route not held to the relay's 1 MB body limit; it has its own 10 MB limit and a 5 minute read deadline.

| Limit | Value | Refusal |
|---|---|---|
| Per file | 10 MB | 413 |
| Per message | 8 attachments | 400 |
| Per uploading agent, kept at once | 200 MB | 413 |
| Relay-wide, kept at once | 1 GB | 507 |
| Free disk after the upload | at least 512 MB | 507 |

Quota is reserved before the file is written (the `Content-Length`, or the full 10 MB when it is absent), so concurrent uploads cannot pass it together. Deleted files stop counting.

To send, name the ids in the send or reply body: `"attachments": [{"id": "..."}]`. The relay accepts only the sender's own finished uploads (403 for another agent's upload, 400 for an unknown id), and each upload rides on one message (409 if it is already on one; upload it again to send it again). The relay fills in `name`, `mime`, and `size` from the upload and ignores any the client sends.

`GET /v1/attachments/{id}` returns the file with its media type, `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, and `X-Tincan-SHA256` (the client checks the bytes against it). It is served to the uploader, to the sender and target of the request that carries it (on the request or on its reply), and to admins. Anyone else gets the same 404 as for an unknown id. A file retention has removed answers 410.

Retention runs when the relay starts and hourly. An upload no message carries is deleted after 24 hours. A file on a request is deleted 7 days after the request reaches a final state (`answered`, `failed`, `declined`, `expired`, or `cancelled`); its metadata row stays, marked deleted, so the message still lists what it carried.

Files live in an `attachments` directory (0700, files 0600) beside the relay database. Uploads and fetches are audited as `attachment_uploaded` and `attachment_fetched`.

## Request groups

A send may include an optional `group` string of 1 to 64 ASCII letters, digits,
underscores or hyphens. The relay stores and echoes it on the request.
Groups do not change identity, parent/chain checks, rate limits, wakes,
allowlists, leases or attachment ownership: each target receives an ordinary
request and requires its own uploads.

`GET /v1/groups/{id}` returns only membership: an array of `{"id": "...", "to": "..."}`
for requests sent by the authenticated caller with that group tag. It includes
no bodies or replies and never marks replies seen. Fetch each result through
`GET /v1/requests/{id}`. An unknown group, or a group with no
requests sent by the caller, returns 404. Recipients and admins do not gain
access to another sender's group through this endpoint.
`GET /v1/capabilities` advertises `"groups": true`.

Clients generate ids prefixed with `group-`, deduplicate targets and cap fan-out
at 8. Older relays ignore the optional field. Clients retain the individual ids
in memory so combined polling still works in the original client instance;
across client restarts, use individual ids or upgrade the relay. Failed sends
are local result entries and are not stored as requests on the relay.
Per-target polling failures appear as `error` text on the combined result entry,
preserving its request id and last known status alongside successful results.
The relay accepts at most 8 requests per sender and group tag; further sends
return HTTP 400. Membership lookups return at most 8 ids and targets.
Clients reconcile relay membership with local send errors, recovering requests
whose send response was lost while retaining errors for targets absent from the
relay. Concurrent group polls preserve the most advanced cached status and replies.
A multi-target MCP notification returns a tool error if any upload or send fails,
with the per-target results included in its content.

Group text output includes the group id and every accepted request id.

Urgent sends have a separate per-sender rolling hourly limit (default 5, configured by `tincan relay --urgent-per-hour`). Exceeding it returns HTTP 429: `urgent limit reached; send without --urgent`. Ordinary sender limits still apply. Sender limits are in memory and reset on relay restart. The optional `urgent` field is also returned on pending request summaries, and a peek carries `"urgent": <n>`, the count of all queued urgent requests (left out when zero), so a channel notice counts them past the 50 that `pending` lists. A send the relay fails to queue (a bad attachment, for example) does not use up an urgent slot. The relay refuses to start with `--urgent-per-hour` below 1. Old clients and relays can ignore this additive field.

### Available client upgrades

`GET /v1/whoami` and both full and `peek=1` responses from `GET /v1/poll` may include `"upgrade_available": "0.5.5"`. This is the release served by the relay's `--dist` VERSION file, distinct from the relay executable's `relay_version`. It is included only when newer than the caller's `X-Tincan-Version` and the dist holds the binary for the caller's `X-Tincan-Platform` (`<os>_<arch>`, for example `darwin_arm64`, sent by every client), so an agent is never told to run a `tincan upgrade` that would fail. The relay reads `VERSION` on each check, so an in-place edit takes effect at once. Missing or invalid versions, development builds, and relays without dist produce no field. Prerelease clients are skipped unless dist itself is a prerelease; comparisons ignore build metadata and accept an optional leading `v`.

A poll with no messages holds until its normal deadline, then returns HTTP 200 with the upgrade field and empty `requests` (or zero `waiting` and `queued` for peek), instead of 204. Populated polls carry the same optional field. `tincan wait` continues waiting on empty polls with an upgrade; it prints the notice when a request or reply ends the wait. The relay repeats it on every response; clients display the actionable notice at most once per process per available version. Unknown fields are safe for older clients to ignore. Notices neither claim requests nor acknowledge replies, and no client upgrades automatically.

## Agent roster

`GET /v1/agents` returns an `agents` array. Each entry optionally includes `queued` (queued or delivered requests), `oldest_queued_at` (their earliest creation timestamp), and `claimed` (requests with a live claim lease). Requests past their expiry and terminal requests are excluded. Zero counts and absent timestamps are omitted. Older clients ignore these additive fields; clients reading an older relay show no backlog. The roster remains visible to joined agents and admins; these counts reveal no request content and do not change the trust model.

## Ping capability

Clients advertise `X-Tincan-Features: ping` on every call, but only polls (`GET /v1/poll`, full or peek) count: they come from the processes that receive requests. The relay admits a ping to a target once one of its polls has advertised support and none of its polls has lacked the header in the last 24 hours, so an older poller running under the same agent name keeps pings away from it. Sends, gets and replies never change this. The state is kept in memory and in the agent store. Versions remain informational, so development builds can advertise support. A `ping` send to a target that does not qualify returns HTTP 409 with an instruction to use `ask`. Older relays reject the unknown kind without delivering it.

A ping has no parent or attachments; its body is empty (up to four bytes are accepted and ignored). Policy still enforces the send rate limit and refuses inferred request parents. The target claims it and replies with status `answered` and body `pong (answered by <surface>, tincan <version>)`. Clients suppress pings from model inboxes. Peek pending entries add optional `kind`, and a peek adds optional `"pings": <n>`, the count of all queued pings (left out when zero), so a listener knows exactly how much ordinary work waits even when more pings are queued than `pending` lists. Pong replies are marked seen when stored and never trigger a reply wake; get-reply and trace still return them.

Polling surfaces are `check_inbox`, the MCP channel loop (`channel`), `inbox`, `wait`, `listen`, `history-serve`, and `web-serve`. The channel loop answers pings without a channel notice, so a Claude Code session pongs without a model turn. Wait and listen loops continue after automatic replies. A pong that fails is retried after the ordinary requests from the same poll have been handed on, never before. A listener answers without invoking its exec command. Such responses demonstrate the client loop is alive, not model execution. `GET /v1/trace?exclude_pings=true` filters before applying the limit; the optional parameter defaults to including all kinds. CLI trace listings omit pings unless `--pings` is supplied; stored traces retain their `ping` kind.
