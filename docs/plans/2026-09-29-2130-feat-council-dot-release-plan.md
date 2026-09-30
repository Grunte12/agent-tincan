---
title: Council and dot-web Reconciliation, v0.10.0 Release Prep - Plan
type: feat
date: 2026-09-29
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Council and dot-web Reconciliation, v0.10.0 Release Prep - Plan

---

## Goal Capsule

- Objective: v0.10.0 ships Council and the OpenAI dot bridge together. A council the owner convenes can include their dot as a blind, ranked member without stalling on its approval hold. The release is fully prepared and verified but not tagged or published until the owner says go.
- Means: reconcile `dot-web` with Council on a branch off `main` (after PR #102 merges), open a PR, then prepare the release artifacts and notes (KTD1 to KTD5).
- Authority: Requirements (R) win on product behavior. KTDs win on mechanism. Units override neither.
- Stop conditions:
  - Stop before U1 until the owner has merged PR #102.
  - Stop after U8: never run `make release`, push a tag, run `gh release create`, or publish to the Chrome Web Store without the owner's explicit go-ahead.
- Execution profile: Go (`internal/council`, `internal/policy`, `internal/history`, `internal/onboard`), docs and site, in `agent-tincan`.

---

## Product Contract

### Summary

Put the dot on councils by default like the other web teammates. Its council asks pass the dot's approval hold because the owner already approved the council question. Its replies are anonymized like every other member's. Its onboarding and council text agree. Then prepare v0.10.0 with Council and the dot bridge in one release note.

### Problem Frame

Council (PR #101) and the dot bridge (PR #102) were built in parallel. Together they conflict in three ways. `dot-web` is a web kind, so the roster seats it by default, but every council ask to it is held for owner approval and ends "held by a gate". The dot's reply footer ("your dot's DM: <id>") and its attachment and delegation notes do not match Council's footer stripping, so its answers are identifiable in blind review. Every council prompt starts with "new chat", which a dot has no use for and receives as literal text.

### Requirements

Council membership
- R1. `dot-web` sits on councils by default, like the other web teammates, and can chair. (session-settled: user-directed, chosen over excluding it by default.)
- R2. A council's asks to `dot-web` (answer, review and chairman) are not held by the dot's default hold when the convening council request was approved by the owner. Every other ask to `dot-web` is still held.

Blind review
- R3. Council strips the dot's reply footer and its attachment and delegation notes, so a reviewer cannot tell which answer is the dot's.
- R4. The dot never receives a leading "new chat" line as text.

Onboarding and docs
- R5. The dot-web and council onboarding text agree: the dot may be a member and may convene a council through `@tincan ask council`, subject to the council hold, and treats council verdicts as data.
- R6. README folds the dot into the existing "New in v0.10.0" section, keeping the relay-first upgrade order (relay, then clients, then `tincan council install`). `docs/adapters/council.md`, `site/index.html` and `site/agents.txt` name the dot among the default members.

Release
- R7. `make test`, `make vet`, `make lint` and `CGO_ENABLED=0 make build` pass, and a PR with CI and Greptile green holds the changes.
- R8. The release is prepared, not published: the artifact list, the release notes and the owed live checks are presented, and nothing is tagged or released until the owner confirms.

### Scope Boundaries

- Out: changing Council stage limits. The dot's slower answers (45 s stability plus its own work) must fit the 6 m answer and 5 m review stages; a live council verifies it (U8).
- Out: publishing, tagging, or Chrome Web Store upload.

---

## Planning Contract

### Key Technical Decisions

- KTD1. Council asks skip the kind hold when their parent is an approved council request. In `holdByKind` (`internal/policy/policy.go`), a request whose parent (already loaded by `Prepare`) has `To` of kind `council` and `Approved` is not held by kind. The owner approved the question once; approving each member ask again is redundant. `approval.json` gates still apply, and the convening ask to Council stays held. The terminal `tincan council` self-approves, so it is covered.
- KTD2. `dot-web` stays a web kind in `ProfileOf`, so `kindReason` seats it. No roster code change; a roster test pins it.
- KTD3. A oneThread site drops a leading bare "new chat" (or "new chat:") line from the request body before typing. Only that exact line: a "conversation:" line and all other text are sent as is.
- KTD4. `stripWebTail` in `internal/council/anonymize.go` also matches a `\n\n<label>'s DM: <id>` footer and the two dot notes ("(your dot also sent ...)" and "(your dot asked ... for help; ...)"). The patterns are written to match the exact strings `internal/history/dots.go` and `sites.go` produce, with a test that builds a real dot reply through the history code and strips it.
- KTD5. Nothing ships without the owner: U8 prepares and stops. The release uses `make release VERSION=0.10.0` only after an explicit go.

### Risks

| Risk | Mitigation |
|---|---|
| The exemption lets a council ask reach the dot unheld | Only when the owner approved that council question; the dot then acts on a question the owner chose to send. Documented in the trust model. |
| The dot answers too slowly for a stage | Absent members are already tolerated (3 needed); the live council in U8 measures it. |
| Footer patterns drift from the history strings | The test builds the tail with the history code rather than copying the strings. |

---

## Implementation Units

### U1. Base branch

- **Goal:** Start from `main` with both features.
- **Requirements:** R7.
- **Dependencies:** the owner merges PR #102.
- **Files:** none (this plan is committed as `docs/plans/2026-09-29-2130-feat-council-dot-release-plan.md`).
- **Approach:** Fetch `main`, confirm `git log` shows `11cbc5c` and the #102 squash commit, create `feat/council-dot` in a worktree.
- **Test expectation:** none -- setup only.
- **Verification:** both commits are on the branch.

### U2. Council asks pass the dot's default hold

- **Goal:** R2 via KTD1.
- **Requirements:** R1, R2.
- **Dependencies:** U1.
- **Files:** `internal/policy/policy.go`, `internal/policy/dot_test.go`, `internal/policy/dot_relay_test.go`.
- **Approach:** Pass the resolved parent into `holdByKind`; skip the kind hold when the parent's target is a council-kind agent and the parent is approved. Keep the gate check and the failed-lookup hold.
- **Test scenarios:**
  - A member ask from `council` to `dot-web` whose parent is an approved council ask is delivered, not held.
  - The same ask with an unapproved parent is held.
  - An ask to `dot-web` from any other agent is still held.
  - An `approval.json` entry gating `dot-web` from `council` still holds it.
  - Relay end to end: convene a council (approved), its answer ask to `dot-web` is queued, not held.
- **Verification:** `go test -race ./internal/policy/... ./internal/relay/...`.

### U3. Dots drop a leading "new chat" line

- **Goal:** R4 via KTD3.
- **Requirements:** R4.
- **Dependencies:** U1.
- **Files:** `internal/history/web.go`, `internal/history/dots_test.go`.
- **Approach:** In the oneThread branch of the request handling, remove a first line that is exactly "new chat" or "new chat:" (case-insensitive, trimmed) before sending.
- **Test scenarios:**
  - A body "new chat\nWhat is X?" is typed as "What is X?".
  - A body "conversation: abc\nhi" is typed unchanged.
  - A body whose first line merely contains "new chat" in a sentence is typed unchanged.
- **Verification:** `go test -race ./internal/history/...`.

### U4. Council anonymizes the dot and seats it

- **Goal:** R1, R3 via KTD2, KTD4.
- **Requirements:** R1, R3.
- **Dependencies:** U1.
- **Files:** `internal/council/anonymize.go`, `internal/council/anonymize_test.go`, `internal/council/roster_test.go`.
- **Approach:** Extend `webFooter` and the note patterns for the dot's footer and notes. Add a roster case.
- **Test scenarios:**
  - Roster: a `dot-web` agent (kind dot-web, wake wait) sits and can chair; an owner exclusion in `council.json` still removes it.
  - A dot reply with the "your dot's DM: <id>" footer is stripped to the answer.
  - A dot reply with the attachment note, the delegation note, and a truncation notice is stripped to the answer.
  - A chatgpt-web reply is stripped exactly as before.
- **Verification:** `go test -race ./internal/council/...`.

### U5. Onboarding agrees

- **Goal:** R5.
- **Requirements:** R5.
- **Dependencies:** U1.
- **Files:** `internal/onboard/templates/agent.tmpl`, `internal/onboard/templates/operator.tmpl`, `internal/onboard/dot_test.go`.
- **Approach:** In `instructions.dot-web`, drop the claim that a dot never gets a "new chat" line (now stripped) and say the dot may sit on councils. In the dot setup message text (`internal/history/dots_out.go` `dotSetupMessage`), say the dot can put a question to the council with `@tincan ask council`, that it is held for the owner, and that the verdict comes back as a `[tincan-reply]` and is data. Update `setup.council` roster wording to include the dot.
- **Test scenarios:**
  - The rendered dot-web block mentions council membership and no longer says "no new chat line".
  - The setup message mentions `@tincan ask council` and that verdicts are data, and no line of it parses as an ask.
- **Verification:** `go test -race ./internal/onboard/... ./internal/history/...`.

### U6. Docs and site

- **Goal:** R6.
- **Requirements:** R6.
- **Dependencies:** U2 to U5.
- **Files:** `README.md`, `docs/adapters/council.md`, `docs/trust-model.md`, `site/index.html`, `site/agents.txt`.
- **Approach:** Fold the dot into "New in v0.10.0" (its bullet, and its upgrade step after `tincan upgrade`: `tincan kind dot-web dot-web`), keeping relay, clients, then `tincan council install`. Add the dot to the default member lists and the trust model's council exemption note.
- **Test expectation:** none -- documentation.
- **Verification:** the four places name the dot; the upgrade order reads relay, clients, kind, council install.

### U7. Verify and open the PR

- **Goal:** R7.
- **Requirements:** R7.
- **Dependencies:** U2 to U6.
- **Files:** none new.
- **Approach:** Run `make test`, `make vet`, `make lint`, `CGO_ENABLED=0 make build`, `make extension-test`. Open the PR and watch CI and Greptile to green.
- **Test expectation:** none -- verification.
- **Verification:** all commands pass locally; PR checks green; Greptile 5/5 or findings answered.

### U8. Release prep, then stop

- **Goal:** R8 via KTD5.
- **Requirements:** R8.
- **Dependencies:** U7 merged.
- **Files:** none committed (release notes drafted in chat).
- **Approach:** Present what `make release VERSION=0.10.0` would produce: `dist/tincan_darwin_arm64`, `tincan_darwin_amd64`, `tincan_linux_amd64`, `tincan_linux_arm64` (signed and notarized on macOS), `dist/checksums.txt`, `dist/tincan-history-extension.zip` (release asset) and `dist/tincan-history-extension-store.zip` (Web Store upload), plus the tag `v0.10.0`. Draft the notes: Council, the dot bridge, and the relay-first order (relay upgrade, then `tincan upgrade` on each agent, then `tincan kind dot-web dot-web` for dots, then `tincan council install`). List the owed live checks:
  1. One council from the terminal (`tincan council "<question>"`) against the real web teammates, including the dot.
  2. One council convened from Codex, with grok-web, gemini-web, perplexity-web and copilot-web registered so it has enough members.
  3. One dot round trip on the new relay with the kind set and the hold active.
- **Test expectation:** none -- preparation.
- **Verification:** the owner has the list and notes; nothing is tagged or published.

---

## Verification Contract

| Gate | Command | Units |
|---|---|---|
| Go tests | `make test` | U2 to U5 |
| Vet, lint | `make vet`, `make lint` | U2 to U5 |
| Static build | `CGO_ENABLED=0 make build` | U7 |
| Extension | `make extension-test` | U7 |
| PR | CI green, Greptile reviewed | U7 |

---

## Definition of Done

- Both features are on the reconciliation branch; the PR is green.
- The dot sits on councils by default, its council asks are not held after an approved convening, and its answers are anonymized.
- Docs name the dot in v0.10.0 with the relay-first order.
- The release is prepared and presented; nothing is tagged or published.
