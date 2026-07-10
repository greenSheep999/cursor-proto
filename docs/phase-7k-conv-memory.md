# Phase 7k — Does Cursor Remember by ConversationId? (Negative result + PrependUserMessages probe)

## Goal

Test whether Cursor's `agent.v1.AgentService` remembers prior turns when the
client reuses the same `AgentRunRequest.ConversationId`. If the server
remembered, we could stop retransmitting history on every turn and cut
input tokens proportionally.

## TL;DR

**The server does NOT remember.** Reusing `ConversationId` alone does not
give the model access to prior turns. `UserMessageAction.ConversationHistory`
is still ignored by the model (confirming phase-7d). The in-band splice is
still needed to make multi-turn context work.

Serendipitous finding: `UserMessageAction.PrependUserMessages` (field 4) IS
fed to the model. Passing prior turns through that channel produced the
correct answer 2/3 runs, using essentially the same input tokens as the
in-band splice. It's a cleaner alternative to the `<prior_conversation>`
wrapper — but not a token saver.

## Experiment

`cmd/test-conv-memory/main.go`. Two-turn conversation, five variants, three
runs each. Turn 1 is always identical (`"Remember the number 42..."`,
Mode=ask, PureMode=true, no history). Turn 2 asks the model to recall the
number and each variant transmits it differently.

| Variant | ConversationId | Wire `ConversationHistory` | In-band splice | `PrependUserMessages` |
|---|---|---|---|---|
| A | reuse turn-1 id | omitted | omitted | omitted |
| B | reuse turn-1 id | populated  | omitted | omitted |
| C | reuse turn-1 id | omitted | populated (baseline) | omitted |
| D | fresh (control) | omitted | omitted | omitted |
| E | reuse turn-1 id | omitted | omitted | populated |

Each turn is run with `PureMode=true` and `Mode=1` (ask) so the model has
no workspace scaffolding to fall back on; if it doesn't remember 42 it
either says so or tries to `grep` for it.

## Results (composer-2.5, 3 runs each)

Raw log: `phase-7k-verify.log`. Structured JSON: `/tmp/conv-memory-results.json`.

| Variant | Pass 2/2 | Turn 2 input tokens (when turn ended cleanly) | Failure mode when it failed |
|---|---|---|---|
| A | 0/3 | 11620 (1 run) | model called `grep` in the other 2 runs — no context |
| B | 0/3 | never reached turn_ended | model called `grep` 3/3 — worse than A |
| C | 3/3 | 11714 | — (baseline; splice works) |
| D | 0/3 | 11620 (2 runs) | model called `grep` 1/3 |
| E | 2/3 | 11716 | 1 run streamed output tokens but no assistant text |

Sample assistant replies for turn 2:

- **A (no context reaches model)**: *"I don't have that number in this
  conversation. There's no earlier message here where you asked me to
  remember one, and I don't have persistent memory across separate chats
  unless it was saved a[s a memory]..."*
- **B (wire ConversationHistory populated)**: *identical apologies when the
  model didn't call grep first.* The wire field changes nothing about the
  model's view.
- **C (splice)**: *"42."* / *"**42** — that's the number you asked me to
  remember."*
- **D (fresh id, control)**: *"I don't have that number in this
  conversation."* — indistinguishable from A/B.
- **E (PrependUserMessages)**: *"42."* (2/3), one silent run.

Token counts confirm the same story:

- A, B, D bottom out around **11620** input tokens — the ~11.6k of Cursor
  boilerplate that ships with every AgentRunRequest even in PureMode. No
  history is reaching the model in any of these variants.
- C is **11714** — +94 tokens for the `<prior_conversation>` wrapper.
- E is **11716** — +96 tokens for the two PrependUserMessages entries.

## Findings

### 1. `ConversationId` reuse does not restore memory

Compare variants A and D: A reuses the turn-1 id, D generates a fresh id
for turn 2. When both reach `turn_ended`, both show the same input-token
count (11620) and the model produces essentially the same "I don't have
that number in this conversation" response. Whatever `ConversationId` is
used for on the server side (billing? logging?), it is not a memory key
for `AgentService.RunSSE`.

### 2. `UserMessageAction.ConversationHistory` is still dropped

Variant B populates the wire field. The model behaves identically to
variant A — same "I don't have that number" or same grep fallback. Server
does not fold `ConversationHistory.messages[]` into the prompt fed to the
model. This confirms the phase-7d observation, now under `PureMode=true`
which strips workspace context that could otherwise mask the effect.

Interestingly variant B triggered `grep` 3/3, worse than variant A. The
wire field seems to nudge the model toward "there's context somewhere,
find it" behaviour without providing that context, making the failure
mode more aggressive.

### 3. `UserMessageAction.PrependUserMessages` (field 4) IS fed to the model

This is the actionable surprise. Populating field 4 with the prior turns
produced correct recall in 2/3 runs, at essentially the same token cost
as the in-band splice. Because the field holds `AgentV1_UserMessage`
protos (no assistant variant), we encoded assistant history as
`"[ASSISTANT]: <content>"` user messages — even that lossy encoding is
enough for the model to pick up the number.

Caveats:

- `PrependUserMessages` doesn't carry a role. If we want to pass a full
  transcript, assistant turns have to be prefixed inline (as this probe
  does) or split up.
- 1 of 3 runs produced 60 output tokens but no captured assistant text.
  Likely a channel/timing thing — need more runs to establish reliability.
- Token cost is nearly identical to splicing (~2 tokens more per turn).
  This is not a bandwidth win; it's a "cleaner wire representation" win.

### 4. No token savings are on the table

The dominant cost of every request is the ~11.6k of Cursor scaffolding
(system prompt, plan-mode reminders, environment info) that ships with
each `AgentRunRequest` in `PureMode`. History (2 short turns in this
test) is 94–96 tokens. Even a 20-turn conversation at 500 tokens per turn
would add ~10k history, dominated by the boilerplate on every turn. A
`SendHistory=false` toggle relying on server memory would save nothing
because there is no server memory to rely on.

## Decision

Do **not** implement `ChatRequest.SendHistory` / `x-cursor-omit-history`.
The premise (server-side memory) is false, so the toggle would trade
correctness for zero token savings.

Keep the executor toggles introduced during this probe
(`OmitSplicedHistory`, `OmitConversationHistoryWire`, `PrependUserMessages`,
`SendToInteractionListener`) — they're small, they're guarded off by
default, and they're useful for future protocol probes. They stay
undocumented in the public surface.

Consider moving the phase-7d splice to use `PrependUserMessages` in a
follow-up (phase-7l or later) if we can prove the reliability delta
against the splice is small. Motivation would be cleanliness, not tokens.

## Files touched

- `executor/chat.go` — added `ChatRequest.OmitSplicedHistory`,
  `OmitConversationHistoryWire`, `PrependUserMessages`,
  `SendToInteractionListener`. Splice now honours the omit flag.
- `executor/chat_build.go` — `buildAgentRunRequest` now honours the wire
  omit flag and populates `PrependUserMessages` /
  `SendToInteractionListener` when set. New `buildPrependUserMessages`
  helper.
- `cmd/test-conv-memory/main.go` — new experiment harness.
- `docs/phase-7k-conv-memory.md` — this file.
- `phase-7k-verify.log` — full three-run log.

## Reproduction

```
cd <cursor-proto>   # any checkout of this repository
CGO_ENABLED=1 go run ./cmd/test-conv-memory -runs 3 -probe-prepend=true -out-json /tmp/conv-memory-results.json
```
