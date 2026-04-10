# chatd redesign handoff

## Purpose

This document is an exhaustive handoff for the next agent working on the chatd
redesign. It summarizes:

- the problem we are solving,
- the design decomposition we agreed on,
- the concrete decisions that were approved,
- the constraints the user explicitly imposed,
- the files we reviewed and the files we produced,
- the open items that remain,
- and the pitfalls that should not be reintroduced unless the user explicitly
  asks to revisit them.

This handoff is planning/design context only. No implementation has been started.

## Current design artifacts in the repo

These are the main documents created during this conversation.

### Canonical split plans

1. `/home/coder/coder/chatd-core-state-machine-plan.md`
   - canonical doc for the **core chat state machine**
   - includes the transition-successor material integrated into it
2. `/home/coder/coder/chatd-stream-state-machine-plan.md`
   - canonical doc for the **stream state machine**
3. `/home/coder/coder/chatd-ownership-state-machine-plan.md`
   - canonical doc for the **ownership state machine**

### Supporting analysis doc

4. `/home/coder/coder/chatd-transition-successor-diagram.md`
   - standalone transition-successor analysis for the current abstract chatd
     model
   - this content has also been integrated into the core state machine plan

### Older monolithic plan

5. `/home/coder/coder/chatd-per-chat-actor-redesign-plan.md`
   - large combined plan that evolved over time
   - **not the preferred entry point anymore**
   - keep it as historical scratch / context, but the split plans above should
     be treated as the canonical planning docs going forward

## Repository source files reviewed during design

These files were inspected while forming the plan and are directly relevant to
future design work:

- `/home/coder/coder/chatd-loop-correctness-model.md`
- `/home/coder/coder/coderd/x/chatd/chatd.go`
- `/home/coder/coder/coderd/x/chatd/chatloop/chatloop.go`
- `/home/coder/coder/coderd/database/queries/chats.sql`
- `/home/coder/coder/coderd/exp_chats.go`
- `/home/coder/coder/coderd/pubsub/chatstreamnotify.go`
- `/home/coder/coder/coderd/pubsub/chatevent.go`
- `/home/coder/coder/codersdk/chats.go`
- `/home/coder/coder/enterprise/coderd/x/chatd/chatd.go`

## High-level problem statement

Current chatd is multi-source and distributed in a way that makes correctness
hard:

- direct SQL row mutations,
- background worker acquisition / heartbeat,
- per-chat pubsub,
- local in-memory fanout,
- enterprise relay callbacks,
- and HTTP handlers that can race.

The redesign goal is to move chatd toward a **single ordered per-chat command
model** so that stale notifications, stale workers, and transport timing stop
being correctness concerns.

The user’s framing was that a chat should behave like a **versioned actor**:

- commands are ordered,
- only one transition executes at a time for a chat,
- long-running side effects report back into the same mailbox,
- subscriber-visible updates are derived in a principled way,
- stale callbacks are fenced by a durable `run_epoch`.

## Three-state-machine decomposition

A key design conclusion was to split the redesign into **three separate state
machines**.

### 1. Core chat state machine

This is the correctness foundation. It owns:

- durable command ingestion,
- serialized command application,
- durable snapshot updates,
- committed message / queue / pending-action mutations,
- command result storage,
- retryable vs terminal command outcomes,
- recovery semantics.

This machine must implement the **full** durable chat semantics, not a reduced
subset.

### 2. Stream state machine

This machine is separate from the core machine. It owns:

- snapshot assembly,
- incremental committed updates over pubsub,
- owner handshake and tail attach,
- tail buffering / catch-up,
- gap detection and resync,
- client-visible ordering during a connected stream.

It should consume outputs from the core machine, not redefine core semantics.

### 3. Ownership state machine

This machine is a runtime ownership/locality layer. It owns:

- owner lease retention,
- hot vs quiescent rules,
- active `StreamChat` pinning,
- no-steal behavior when a fresh owner exists,
- ownership release,
- heavy working-set residency policy.

This machine should not change core chat semantics. It only affects ownership
lifetime and locality.

## Most important user-approved design decisions

These decisions were explicitly discussed and approved.

### A. No partition hashing

The user explicitly rejected partition hashing / partition ownership.

Approved direction:

- use a **per-chat ownership lease** instead,
- all replicas may race to claim a chat,
- only one replica wins and continues,
- ownership is visible in the DB via `worker_id` and `heartbeat_at`.

### B. No `chat_stream_events`

The user explicitly preferred removing the proposed durable event-log table.

Approved direction:

- **do not add `chat_stream_events`**,
- use `chats.last_applied_command_id` as the durable committed-state watermark,
- use snapshot refetch / consistent snapshot reads plus pubsub deltas instead of
  a durable ordered event log.

### C. Do not persist raw in-flight `message_part` deltas

Approved direction:

- in-flight token/message-part streaming stays **ephemeral**,
- owner keeps a bounded in-memory buffer for reconnect / attach,
- relay remains necessary for the active live tail.

### D. Keep relay for the active tail

Relay is still required, but only for the **ephemeral live tail** of an active
run.

Approved direction:

- non-owner replicas should serve the durable committed prefix from Postgres,
- then reach the current owner for the active ephemeral tail,
- relay is not the source of truth for committed state.

### E. Processed commands should not stay in the DB indefinitely

The user explicitly asked to remove processed commands from the DB as soon as
possible.

Approved direction:

- `chat_commands` is a **durable pending-work queue**, not a retained history
  log,
- once a command is conclusively processed, it should be removed from
  `chat_commands`.

### F. Add short-lived `chat_command_results`

Because processed commands are deleted, synchronous waits and retries need a
separate result store.

Approved direction:

- add `chat_command_results` as a short-lived result / dedupe table,
- this is the source of truth for synchronous waiting,
- results expire by TTL,
- working assumption in the plan: **15 minutes** TTL,
- `idempotency_key` is **required for external commands**.

### G. Retry policy

Approved direction:

- use a dedicated **terminal-error type**,
- if execution returns that type, treat it as non-retryable,
- all other execution failures are retryable,
- retry retryable failures up to **10 attempts**,
- after the cap is reached, convert to terminal failure and write an error
  result.

### H. Pubsub committed delta should embed applied command info

The user did **not** want listeners to have to query the DB again just to know
what command finished.

Approved direction:

- committed pubsub deltas should include an embedded `applied_command` payload,
  not just `applied_command_id`.

The plan currently says the embedded `applied_command` should include at least:

- `id`
- `chat_id`
- `kind`
- `source`
- `status`
- `result_payload` or `error_payload`

### I. Stream attach optimization when owner is ahead

Important approved stream decision:

If the owner’s `draft_base_command_id` is ahead of the durable snapshot’s
`last_applied_command_id`, **do not immediately refetch the whole snapshot**.

Approved direction:

- send the durable snapshot to the client immediately,
- start buffering the owner’s tail,
- wait for pubsub deltas to advance the in-memory snapshot watermark,
- attach the tail only after the watermark catches up.

### J. Sticky ownership / pinning is explicit and desired

A major late decision was that sticky ownership is wanted, not incidental.

Approved direction:

- chats should stay on one owner while hot,
- active `StreamChat` connections should pin the current owner,
- if a chat is unowned, the first active stream connection may claim it,
- if a chat already has a fresh owner, later stream connections **must not**
  steal ownership,
- owner pinning and heavy working-set residency are separate concerns.

### K. No feature flag / no long-lived dual-path rollout

The user explicitly rejected a feature-flagged dual-runtime rollout at the plan
level.

Important nuance:

- the implementation plan is only a guide for the engineer,
- intermediate commits do not need to be individually mergeable,
- the old implementation may be removed early and replaced during the refactor,
- the final branch is expected to be complete before merge.

The plan was updated to reflect this.

## Core chat state machine decisions

These are the key decisions specific to the core machine.

### Durable state

Core durable structures:

- `chats`
  - `status`
  - `worker_id`
  - `heartbeat_at`
  - `run_epoch`
  - `last_applied_command_id`
  - `pending_action`
- `chat_commands`
  - pending queue only
- `chat_command_results`
  - short-lived result/dedupe store
- `chat_messages`
- `chat_queued_messages`

### Serialized execution assumption

The core machine does **not** define ownership policy internally.
It assumes one external guarantee:

> next-command application is serialized per chat.

This was an important clarification. The core machine needs serialized execution,
but the ownership state machine owns the policy of how long the same replica keeps
ownership.

### Linearization point

An applied command should linearize at one DB transaction that:

1. locks the chat snapshot row (or creates it for `CreateChat`),
2. verifies the current executor may apply the command,
3. reads the next pending command for the chat,
4. verifies it is the next expected command,
5. applies the durable transition,
6. advances `last_applied_command_id`,
7. appends follow-up internal commands if needed,
8. writes a short-lived result row if needed,
9. deletes the processed command row.

### Outcome model

Each command ends in exactly one of:

- successfully processed,
- terminally failed,
- retryable failure.

### Full scope, not reduced scope

The user explicitly did **not** want a reduced first implementation of the core
machine.

The first implementation target for the core machine should include full durable
semantics:

- create,
- direct send,
- enqueue,
- queued delete,
- queued promote,
- edit,
- start run,
- repeated step commit,
- interrupt,
- enter `requires_action`,
- submit tool results,
- auto-promote,
- archive/unarchive,
- stale recovery.

## Stream state machine decisions

Even though the user asked not to keep revisiting the stream-state discussion,
a substantial amount of stream design was still settled and written into the
stream plan.

### Snapshot-first model

Approved direction:

1. subscribe to pubsub first,
2. read one consistent durable snapshot,
3. send that snapshot immediately,
4. if running, perform an owner handshake,
5. attach, buffer, or resync based on handshake + watermark rules.

### Durable anchor

The stream machine is anchored by:

- `chats.last_applied_command_id`

### Owner handshake payload

Approved payload now includes:

- `worker_id`
- `run_epoch`
- `draft_base_command_id`
- `tail_open`
- `next_part_seq`
- `buffered_parts`
- `part_seq` on parts

### Tail attach rules

Approved rules:

#### Exact match
If owner handshake base equals snapshot watermark:
- attach immediately.

#### Owner ahead
If owner handshake base is greater than snapshot watermark:
- send snapshot immediately,
- buffer tail,
- wait for pubsub catch-up,
- attach only after watermark reaches the owner’s base.

#### Stale / incompatible handshake
If owner base is lower than snapshot watermark, or owner/run mismatch occurs:
- reject the handshake,
- restart attach logic,
- resync if needed.

### Incremental delta application

Connected streams should:

- maintain an in-memory durable snapshot,
- apply contiguous deltas in memory,
- ignore stale/duplicate deltas,
- resync on gaps.

### Request-result wake model

For synchronous external waiters, approved direction is:

- durable truth: `chat_command_results`
- fast wake-up: pubsub result-ready notification keyed by `command_id`
- correctness fallback: periodic DB recheck until timeout/cancellation

Recommended request-side flow in the plan:

1. enqueue command and get `command_id`
2. register local waiter by `command_id`
3. immediately check `chat_command_results` once to close the fast-completion
   race
4. if not found, wait on:
   - local waiter signal from pubsub,
   - periodic timer for DB recheck,
   - request cancellation
5. on wake, reread `chat_command_results` and return

## Ownership state machine decisions

### Hot rules

A chat is considered hot while any of these are true:

- an LLM run is active
- immediately runnable internal commands exist
- queue follow-up can execute immediately
- an ephemeral live tail is active or buffered
- known local work exists that the same owner can continue without another
  global claim cycle

### Pin rules

A chat is pinned while at least one active `StreamChat` connection exists.

Approved policy:

- if unowned, first active stream connection may claim it
- if already owned by a fresh owner, later connections pin the existing owner
  rather than moving the chat
- active viewers should not be forced to switch replicas unnecessarily

### Local fast path

This was discussed carefully.

Approved meaning of “local fast path”:

- internal follow-up work is still durably enqueued,
- the same owner may continue draining the durable queue locally,
- but it must still process the **next pending command in durable queue order**.

This preserves correctness even if another replica enqueues a command between
self-loop turns.

### Ownership pinning vs heavy residency

Approved separation:

- ownership can remain pinned while stream pins exist,
- heavy prompt/history/runtime working set only needs to remain resident while
  the chat is hot or tail buffering is active,
- idle-but-pinned chats may evict heavy working state without releasing
  ownership.

## Important constraint / pitfall list for the next agent

These items were explicitly decided and should **not** be casually revisited.

### Do not reintroduce partition hashing

The user explicitly does not want partition ownership as the execution model.

### Do not reintroduce `chat_stream_events`

The user explicitly prefers snapshot + delta + relay tail over a durable event
log table.

### Do not suggest persisting raw streaming parts

The user explicitly rejected persisting in-flight `message_part` deltas.

### Do not suggest feature-flagging the runtime split

The user explicitly does not want a long-lived dual implementation path in the
plan.

### Do not allow later stream connections to steal a fresh owner

This was explicitly discussed and approved.

### Do not interpret sticky ownership as permission to bypass durable queue order

The local fast path must still consume commands in durable queue order.

## Important unresolved / notable points still in the design

These are not necessarily blocked, but they are important places for the next
agent to pay attention.

### 1. `ManualPromote` vs `requires_action`

This is explicitly called out in `chatd-transition-successor-diagram.md` and in
`chatd-core-state-machine-plan.md`.

Problem:

- abstract `ManualPromote(qid)` only requires `qid ∈ Q`
- if applied while `status = requires_action`, it sets `status := pending` but
  does not clear `pendingCalls`
- that violates invariant `I5`

The redesign must explicitly choose one of:

- reject `PromoteQueued` while `status = requires_action`, or
- redefine it so it also clears/closes pending calls, or
- give it a different meaning in the actor model.

This was not fully resolved in the conversation.

### 2. Stream-state details may still need tightening

The user at one point asked not to keep revisiting the stream-state internals,
so the plan does not necessarily have every micro-detail locked down. But the
main architectural choices above were approved.

### 3. Combined plan file is not canonical anymore

The split plans are the documents that should be extended going forward.

## Files that should be treated as canonical going forward

If the next agent needs to continue design work, start here:

1. `chatd-core-state-machine-plan.md`
2. `chatd-stream-state-machine-plan.md`
3. `chatd-ownership-state-machine-plan.md`
4. `chatd-transition-successor-diagram.md`

Use the old monolithic plan only as a scratch/history reference.

## Suggested next-agent reading order

1. `chatd-core-state-machine-plan.md`
2. `chatd-transition-successor-diagram.md`
3. `chatd-stream-state-machine-plan.md`
4. `chatd-ownership-state-machine-plan.md`
5. `chatd-loop-correctness-model.md`
6. `coderd/x/chatd/chatd.go`
7. `coderd/x/chatd/chatloop/chatloop.go`
8. `coderd/database/queries/chats.sql`
9. `coderd/exp_chats.go`
10. `enterprise/coderd/x/chatd/chatd.go`

## What the next agent should be ready to help with

Depending on where the user wants to go next, the likely useful follow-ups are:

- tighten any remaining unresolved semantics in the core machine,
- refine the stream machine protocol details,
- sanity-check the ownership / pinning model against current source code,
- turn the design docs into implementation-ready schema/query change lists,
- or start implementation planning on the chosen state machine.

## Summary in one paragraph

The redesign we converged on replaces current chatd with three separate state
machines: a **core chat state machine** built around a durable per-chat command
queue plus short-lived command results and a durable snapshot watermark; a
**stream state machine** that serves an initial committed snapshot, applies
incremental committed deltas, and stitches in an owner-routed ephemeral tail;
and an **ownership state machine** that keeps ownership sticky while a chat is hot
or actively viewed, without letting later viewers steal a fresh owner. The user
explicitly rejected partition hashing, a durable stream event table,
persisted in-flight message parts, and a feature-flagged dual-runtime rollout.
The split plan files in the repo root are the canonical design docs now.
