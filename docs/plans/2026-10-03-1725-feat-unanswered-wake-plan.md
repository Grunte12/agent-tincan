---
title: Unanswered Wakes - Plan
type: feat
date: 2026-10-03
topic: unanswered-wake
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Unanswered Wakes - Plan

---

## Goal Capsule

- Objective: When the relay wakes an agent and that agent does not check in, the owner and the agents asking it find out within minutes, instead of requests silently piling up.
- Means: the relay remembers the last wake it sent each relay-woken agent and whether the agent polled after it, and reports a stuck agent in the roster, on send, in `tincan top` and in `tincan doctor` (KTD1-KTD4).
- Product authority: the owner. Changing how any platform is woken, retrying through another channel, or alerting the owner outside Tincan is not active scope.
- Stop conditions: stop and ask if the signal needs a schema change beyond one small table or column, or if it would change wake sending itself.
- Who finishes: an implementing agent lands U1-U4 in one PR; the release ships with the next version.
- Open blockers: none.

---

## Product Contract

### Summary

The relay notices when a wake it sent did not lead to a check-in. A webhook or email agent that was woken and has not polled within a grace period shows as unanswered in `tincan agents`, `list_agents` and `tincan top`. Agents asking it are told it was woken and has not answered, and `tincan doctor` on an admin device names it and the last wake result. Nothing about how wakes are sent changes.

### Problem Frame

On 2026-10-03 GrokBot stopped answering for hours. The relay sent its webhook on every request, and the Grok Bot app received it, but the app's routines were failing ("Activity task failed"), so no session ever started and GrokBot never polled. Tincan showed GrokBot as an ordinary offline webhook agent, which is its normal state between wakes. Requests from teammates sat queued, a ping timed out, and the only way anyone learned something was wrong was by messaging GrokBot directly in its app.

The relay already has the facts. It records `woke` and `wake_failed` audit rows for every webhook and email wake (`internal/wake/wake.go`), and it knows each agent's last poll. Nothing joins the two: the audit rows have no trace id and no reader, and the only "missed check-in" signal, `overdue`, exists for `schedule` agents alone. `tincan top` deliberately never flags an offline webhook or email agent with queued work, because the relay is expected to wake it (`internal/cli/top.go`). That assumption is exactly what failed.

The same gap exists for email-woken sandboxes (Instinct's loop stalls) and any webhook host whose receiver breaks, so this is a Tincan product gap, not a Grok Bot one.

### Key Decisions

- Detect, do not fix. Tincan reports a stuck wake path; it does not retry through another channel or change wake methods. The fault lives in the agent's platform, and only the owner can fix it there. Governs R1-R6.
- Unanswered is measured against the agent's own wake: a wake was sent (or failed to send) and no poll followed within a grace window. An agent that is merely offline with nothing waiting is never flagged. Governs R1, R2.
- The relay computes the state and clients only render it, as with schedule facts, so every client and `list_agents` agree. Governs R3, R4.

### Requirements

Detection

- R1. For each agent the relay wakes (webhook or email), the relay keeps the time and result of the last wake it sent and whether the agent polled after it.
- R2. The agent is unanswered when a wake was sent or failed at least the grace window ago (default 10 minutes, a relay flag) and the agent has not polled since; the first poll after it clears the state.
- R3. A failed wake send (`wake_failed`: an HTTP error, a timeout) counts immediately as unanswered, with the error kept for display.

Reporting

- R4. The roster marks an unanswered agent with when it was last woken and the last wake result; `tincan agents` and `list_agents` show it on the agent's own line.
- R5. An ask to an unanswered agent tells the sender it was woken at a given time and has not checked in, so the reply may not come until the owner fixes it; the request is still queued as before.
- R6. `tincan top` flags unanswered agents as needing attention ahead of ordinary offline ones, and `tincan doctor` on an admin device lists unanswered agents with their last wake result.

Compatibility

- R7. Older clients ignore the new optional fields; a newer client against an older relay shows nothing new. Wake sending, debounce, the hourly cap and retries are unchanged.

### Acceptance Examples

- AE1. Covers R2, R4, R6. Given grokbot (webhook) has a queued request, the relay sent its webhook 12 minutes ago with a 200, and grokbot has not polled since: `tincan agents` shows grokbot as unanswered since that wake, and `tincan top` lists it under attention.
- AE2. Covers R2. Given the same, grokbot then polls: the unanswered mark disappears on the next roster read.
- AE3. Covers R3. Given the webhook POST returned 502 twice (the send and its retry): grokbot shows unanswered at once with "webhook returned 502".
- AE4. Covers R5. Given grokbot is unanswered, claude-code asks it something: the ask is queued and the reply text says grokbot was woken at 16:40 and has not checked in.
- AE5. Covers R2. Given muse (wait) or codex (command) is offline with work waiting: nothing changes; the relay does not wake them, and QUEUED-OFFLINE already covers them.

### Scope Boundaries

Deferred for later

- Alerting the owner outside Tincan (a push, an email, a message to a chosen teammate) when an agent stays unanswered.
- Falling back to a second wake method.
- Wake history beyond the last wake per agent.

Outside this product's identity

- Fixing a platform's wake receiver. Tincan reports it; the platform owns it.

### Dependencies / Assumptions

- Relay-side wake state lives in memory today (the hourly cap's `sent` map). Persisting the last wake per agent is assumed small (one row per agent) so a relay restart does not hide a stuck agent; planning decides memory vs store (KTD1).
- A poll is the right proof of life: a woken agent that runs at all calls `poll` or `inbox`. Ping already relies on this.

### Sources / Research

- Live incident 2026-10-03: GrokBot's "Tincan wake" and "Keep Tailscale always on" routines both failed with "Activity task failed" from about Oct 2 evening; another bot's routine in the same app also stopped running on Oct 3.
- `internal/wake/wake.go` (`fire`, `send`, `do`, `record`, `sent` map); `internal/relay/server.go` (`handleAgents`, `scheduleTarget`, `recipientTarget`); `internal/client/render.go` (`scheduleHint`, `OverdueNote`); `internal/cli/top.go` (attention scores); `internal/cli/doctor.go`.
- Prior pattern: `docs/plans/2026-09-28-1917-feat-scheduled-agents-plan.md` (relay-computed `overdue`, send-time `target`).

---

## Planning Contract

### Key Technical Decisions

- KTD1. The waker records, per agent, the last wake time, its result (ok, or the error string) and is consulted together with the relay's last-poll time. Keep it in the waker's memory plus one small store table (agent, woken_at, result) so a restart keeps the signal; the relay restart path already re-wakes agents with queued work, which refreshes it.
- KTD2. `AgentInfo` gains optional `woken_at`, `wake_result` and `unanswered` fields (omitempty), filled in `handleAgents` from KTD1 and the last poll; the grace window is a relay flag `--wake-grace` (default 10m) beside `ScheduleGrace`.
- KTD3. The send response's existing `target` object gains the same three fields for a relay-woken recipient, and `client` renders them next to the schedule hint, reusing that path rather than adding a new one.
- KTD4. `tincan top` scores unanswered between QUEUED-OFFLINE and STALE and stops exempting webhook/email agents when they are unanswered; `doctor` adds one admin-only check listing unanswered agents.

### Assumptions

- No `docs/solutions/` learnings exist; the scheduled-agents work is the closest precedent.

---

## Implementation Units

### U1. Remember the last wake per agent

Goal: the waker keeps each relay-woken agent's last wake time and result, in memory and in a small store table.

Requirements: R1, R3, R7; KTD1.

Files: `internal/wake/wake.go`, `internal/wake/wake_test.go`, `internal/store/store.go` (migration + get/set), `internal/store/*_test.go`.

Test scenarios:
- A successful webhook send records woken_at and result ok; a send whose retry also fails records the error text.
- The record survives a store reopen.
- Debounced or capped (`wake_skipped`) wakes do not overwrite the last real send.

### U2. Compute unanswered on the relay and expose it

Goal: the roster and the send response carry woken_at, wake_result and unanswered.

Requirements: R2, R4, R5, R7; KTD2, KTD3.

Files: `internal/relay/server.go`, `internal/cli/relay.go` (flag), `internal/client/relay.go`, `internal/envelope/*` if `target` lives there, `internal/relay/server_test.go`, `internal/client/relay_test.go`.

Test scenarios:
- Covers AE1, AE2. Wake at T, no poll, roster at T+grace+1m shows unanswered; a poll clears it.
- Covers AE3. A failed send shows unanswered immediately with the error.
- Covers AE5. A command, wait or channel agent never gets these fields.
- Covers AE4. An ask to an unanswered webhook agent returns `target` with the fields.

### U3. Render it in agents, list_agents, ask, top and doctor

Goal: every surface shows an unanswered agent the same way.

Requirements: R4, R5, R6; KTD3, KTD4.

Files: `internal/client/render.go`, `internal/cli/agent.go`, `internal/mcpserver/server.go`, `internal/cli/top.go`, `internal/cli/doctor.go`, and their tests.

Test scenarios:
- `formatAgents` and `list_agents` show "woken 12m ago, no check-in (webhook ok)" on the agent's line, after good_at stays last.
- The ask reply text names the wake time and says the request is still queued.
- `top` puts an unanswered webhook agent above ordinary offline ones.
- `doctor` on an admin device lists unanswered agents; on a non-admin device the check is skipped.

### U4. Docs

Goal: README (Wake methods, Last seen, top), `docs/protocol.md` roster and send fields, and `site/agents.txt` describe unanswered wakes, and the Grok Bot adapter doc says what to check when GrokBot shows unanswered (the app's routine run status).

Requirements: R4-R6.

Test expectation: none -- docs only.

---

## Verification Contract

| Gate | Command | Applies to |
|---|---|---|
| Tests with race detector | `make test` | U1-U3 |
| Vet and lint | `make vet`, `make lint` | all |
| Manual (owner) | With GrokBot's routine paused, send it an ask: within 10 minutes `tincan top` flags it and the ask reply says it was woken and has not checked in; resume the routine and the flag clears after its next poll | R2, R5, R6 |

---

## Definition of Done

- U1-U4 merged; `make test`, `make vet` and `make lint` pass.
- AE1-AE5 covered by tests.
- Wake sending behavior unchanged (existing wake tests pass untouched).
- No dead-end code left in the diff.
