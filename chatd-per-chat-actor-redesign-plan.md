# chatd per-chat actor redesign plan

## Status

Draft for review only. Do not start implementation until this plan is
explicitly approved.

## Context

`chatd` already behaves like a state machine, but execution is split across SQL,
worker heartbeats, pubsub, relay, in-memory fanout, and HTTP handlers that can
race one another.

The redesign target is a **single ordered per-chat command model** where every
mutation enters one durable command queue, one replica owns execution at a time,
stale callbacks are fenced by `run_epoch`, and streaming uses a durable
snapshot plus an owner-routed ephemeral tail.

## Decisions locked in for this revision

- Do **not** use partition hashing / partition ownership.
- Use a **per-chat ownership lease** via `worker_id` and `heartbeat_at`.
- Do **not** add a durable per-chat event-log table.
- Do **not** persist raw in-flight `message_part` deltas.
- Keep relay for the active ephemeral tail of a running chat.
- Use `chats.last_applied_command_id` as the durable snapshot watermark.
- Keep connected streams efficient by applying **incremental committed deltas**
from pubsub to an in-memory snapshot.
- When the owner is ahead of the snapshot, send the snapshot immediately,
buffer the tail, and attach it only after the in-memory snapshot catches up.
- Keep chats pinned to one owner replica while they remain hot or while an
  active `StreamChat` connection exists, so repeated self-loop transitions and
  active viewers do not bounce across replicas.

## Goals

1. One durable command ingress for every chat mutation.
2. One owner replica at a time executes commands for a chat.
3. Fence every long-running effect with a durable `run_epoch`.
4. Preserve current public API behavior wherever possible.
5. Make connected streams race-free without persisting token-level events.
6. Replace the legacy implementation directly instead of maintaining a long-lived dual path.

## Non-goals

1. Do not fully event-source chat state on day one.
2. Do not add Kafka, NATS, or another external broker.
3. Do not persist raw in-flight `message_part` deltas.
4. Do not remove relay from active-run streaming.
5. Do not refetch the full snapshot on every committed pubsub update.
6. Do not retain processed commands indefinitely once they are conclusively processed.

## Durable vs ephemeral state

### Durable state in Postgres

- `chats`
  - `status`
  - `worker_id`
  - `heartbeat_at`
  - `run_epoch`
  - `last_applied_command_id`
  - `pending_action`
  - existing snapshot fields needed for prompt building and API responses
- `chat_commands`
  - durable pending queue of external and internal commands
- `chat_command_results`
  - short-lived processed results for synchronous waits and idempotent retries
- `chat_messages`
  - committed history
- `chat_queued_messages`
  - committed queue projection

### Ephemeral owner-local state

- in-memory actor instance for a claimed chat
- live `chatloop.Run` goroutine
- cancellation handle
- bounded replay buffer of recent in-flight `message_part`s
- `draft_base_command_id`
- `part_seq`
- relay connections and local live subscribers

## Proposed execution model

### 1. Durable command queue

Add `chat_commands` as the only durable ingress for chat mutations.

In this revision, `chat_commands` is a **durable pending-work queue**, not an
indefinitely retained history log. Once a command has been conclusively
processed, it should be removed from `chat_commands`.

Every mutating operation becomes a command append first. After migration, HTTP
handlers and effect callbacks must not directly mutate `chats`,
`chat_messages`, or `chat_queued_messages`.

Each command row should include at least:

- `id BIGSERIAL`
- `chat_id`
- `kind`
- `payload JSONB`
- `source` (`external`, `effect`, `recovery`, `system`)
- `idempotency_key`
- `created_by`, `created_at`
- retry/backoff metadata needed while the command is still pending

### 1a. Short-lived command results and idempotency

Because processed commands are deleted from `chat_commands`, synchronous HTTP
waiters and retried requests cannot rely on the command row itself as a durable
result record.

Add `chat_command_results` (or an equivalently scoped result/dedupe store) for
short-lived processed results. It should contain at least:

- `command_id`
- `chat_id`
- `idempotency_key`
- `status`
- `result_payload`
- `error_payload`
- `created_at`
- `expires_at`

This table exists only for waiting for a just-enqueued command to finish,
deduplicating retried external requests, and returning the same response on
cross-replica retries. It should be cleaned up by TTL and is not intended to
become a second permanent history log.
Retryable command failures are retried up to 10 attempts. After the cap is reached, the command is treated as terminal, an error result is written, and the command is deleted.

For request-time waiting, `chat_command_results` is the source of truth.
Pubsub should only provide a fast wake-up hint that a result row may now exist.
For external commands, `idempotency_key` is required. Results expire by TTL; a default of 15 minutes is the working assumption for this plan.

Initial external commands:

- `CreateChat`
- `SendMessage`
- `EditMessage`
- `DeleteQueued`
- `PromoteQueued`
- `SubmitToolResults`
- `Interrupt`
- `Archive`
- `Unarchive`

Initial internal commands:

- `StartRun`
- `RunStepCommitted`
- `RunInterrupted`
- `RunFailed`
- `RunRequiresAction`
- `AutoPromoteHead`
- `RecoverRunning`
- `RecoverRequiresAction`

### 2. Actor-owned snapshot on `chats`

Keep `chats` as the durable snapshot row and extend it with:

- `run_epoch BIGINT NOT NULL DEFAULT 0`
- `last_applied_command_id BIGINT`
- `pending_action JSONB`

`last_applied_command_id` is the durable answer to:

> which committed state changes are included in this snapshot?

That watermark replaces the need for a durable per-chat event log table.

### 3. Per-chat owner lease via `worker_id` and `heartbeat_at`

Ownership model:

- all replicas may race to claim a chat when work appears,
- one replica wins by updating `worker_id` and `heartbeat_at`,
- the winner becomes the current owner for command-queue execution,
- the owner keeps heartbeating while active work exists,
- other replicas use `worker_id` for relay routing,
- a stale owner may be replaced by another replica.

Important semantic change:

- in this design, `worker_id` means **current owner of chat processing while
active work exists**, not only “worker currently inside an LLM step”.

### 4. Queue stays as a projection

Keep `chat_queued_messages` for API compatibility, but make it actor-owned.

That means:

- only actor transitions mutate it,
- HTTP handlers stop mutating it directly,
- auto-promotion becomes an explicit actor transition,
- queue/history movement becomes atomic with the rest of the command apply.

### 5. Wake, claim, and poll fallback

Once a command is enqueued, the system should start processing immediately, but
correctness must not depend on pubsub delivery. The same rule applies to
request-time result waiting: pubsub wakes waiters quickly, but the durable
result row is still the real source of truth.

Mechanism:

1. append the command to `chat_commands`
2. publish a best-effort wake containing `chat_id`
3. all replicas may race to claim the chat
4. one wins and starts / continues the owner loop

Fallback:

- every replica also polls for chats with unapplied commands and no fresh owner
lease
- missed or delayed wakes are recovered by polling

So:

- command-queue rows are the correctness source of truth
- wake pubsub is only a latency optimization

### 6. One owner loop per active chat

The winning replica creates or reuses a local actor for the chat.

That actor:

- loads the durable snapshot
- reads next unapplied commands for that chat
- applies one transition at a time
- stores command results for waiting HTTP handlers
- launches active effects when required
- publishes committed deltas after successful apply
- maintains the bounded in-memory live-tail buffer
- updates its in-memory working set incrementally instead of rebuilding full
history after every self-loop transition

As long as the chat remains hot or has an active stream pin, the owner keeps
its lease.

### 7. Owner session policy: hot vs quiescent

Sticky ownership should be an explicit part of the redesign, but it belongs to
runtime policy rather than to the core durable chat-state machine.

The durable state machine defines what the chat means. The owner session policy
controls how long one replica keeps execution locality while the chat keeps
making immediately runnable progress.

#### Hot owner session

A claimed chat enters a hot owner session when a replica becomes its owner and
there is active or immediately runnable work.

A chat is **hot** while any of these are true:

- an LLM run is active
- immediately runnable internal commands exist
- queue follow-up can be executed immediately
- an ephemeral live tail is active or buffered
- there is already known local work that the same owner can continue without a
  new global claim cycle

A chat is also **pinned** while at least one active `StreamChat` connection
exists for that chat.

While hot or pinned:

- the same owner should keep the lease across repeated transitions
- self-generated internal commands should take the local fast path after durable
  enqueue, but still be consumed in durable queue order
- the owner should update its in-memory working set incrementally while hot
- relay clients should stay attached to the same owner
- a new stream connection on another replica should not steal ownership from a
  fresh owner

If the chat is unowned, the replica serving the first active stream connection
may claim it. If the chat already has a fresh owner, later connections pin the
existing owner instead of moving the chat.

This is the common-case performance path for repeated `R0 -> R0 CommitStep`
loops and for active viewers that should not be forced to switch replicas.

#### Quiescent state

A chat is **quiescent** when none of the hot conditions hold.

Typical quiescent points are:

- `waiting` with no backlog and no live tail
- `error` with no immediate follow-up work
- `requires_action` waiting on external input

Quiescent does not necessarily mean unowned. An idle chat may remain pinned by
an active stream connection even after heavy runtime work has stopped.

#### Lease release rule

Do not release the owner lease after every transition.

Release it only after:

- the active run has stopped
- no immediately runnable internal command remains
- no immediate queue follow-up remains
- no buffered / active live tail still needs serving
- no active `StreamChat` pin remains

#### Memory / residency rule

Ownership pinning and heavy working-set residency should be treated separately.

- keep the owner lease pinned while stream pins exist
- keep the heavy prompt/history/runtime working set resident only while the chat
  is hot or tail buffering is active
- allow the heavy working set to be evicted when the chat is idle-but-pinned

This preserves replica locality for active viewers without forcing maximum
memory residency forever.

#### Why this is necessary

Without sticky owner sessions, a naive per-transition ownership model would
rebuild prompt/history from the DB on every self-loop turn, force relay
reconnect churn, and move active viewers between replicas unnecessarily. This
policy lets many logical transitions execute under one owner session while
preserving the same durable command model.

### 8. Effect bridge with `run_epoch`

`runChat` and `chatloop.Run` must stop directly persisting committed state.

Instead:

- the actor handles `StartRun`
- increments `run_epoch`
- marks the chat running
- launches the effect
- effect callbacks append internal commands tagged with `chat_id` and
`run_epoch`
- only the actor commits messages, queue state, `pending_action`, and snapshot
changes
- callbacks with a stale `run_epoch` are dropped

### 9. Publish committed deltas, not a durable per-chat event log

After each applied command, the owner publishes a **committed-state delta** over
pubsub.

This delta is **not** durable. It exists only so connected stream handlers can
advance their in-memory snapshot efficiently.

Each committed delta should carry at least:

- `applied_command`, embedded in the pubsub payload
- `run_epoch`
- `worker_id` if it changed
- the committed mutation payload needed to update the in-memory snapshot:
  - appended committed messages
  - status change
  - queue change
  - `pending_action` change
  - edit / timeline reset
  - error / action-required metadata

`applied_command` should include enough information that listeners do not need a
second database read just to learn what command finished and what its result
was. In particular it should include at least:

- `id`
- `chat_id`
- `kind`
- `source`
- `status`
- `result_payload` or `error_payload`

One applied command should correspond to one atomic delta batch from the stream
handler’s point of view.

Commands with no user-visible effect may still need to publish a no-op watermark
advance so streams can catch up to the owner’s `draft_base_command_id`.

## StreamChat design

The stream stays **snapshot-first**, but ordering is made explicit.

### Step 1: subscribe to pubsub first

Before reading the database snapshot, the serving replica subscribes to the
chat’s pubsub channel.

That preserves the current subscribe-first guarantee: a commit that happens
while setup is in progress is still observable.

### Step 2: read one consistent durable snapshot

Read the initial durable snapshot in one consistent DB snapshot, including at
least:

- `chats.status`
- `chats.worker_id`
- `chats.run_epoch`
- `chats.last_applied_command_id`
- committed `chat_messages`
- committed `chat_queued_messages`
- committed `pending_action`

Call the durable watermark `S = last_applied_command_id`.

### Step 3: send the durable snapshot immediately

Always send the durable snapshot immediately.

Even if the owner is already ahead in memory, the snapshot is still a durable
committed prefix.

### Step 4: if not running, stay snapshot-driven

If `status != running`, there is no live tail to attach.

The connection remains snapshot-driven and applies committed pubsub deltas to
its in-memory snapshot.

### Step 5: if running, perform an owner handshake

If the snapshot says the chat is running, the serving replica connects to the
owner from `worker_id` and requests a live-tail handshake.

The handshake must return at least:

- `worker_id`
- `run_epoch`
- `draft_base_command_id`
- buffered in-flight parts
- future live parts stream
- `part_seq`

`draft_base_command_id` means:

> the current in-flight draft begins immediately after durable committed state
> version N.

## Tail attach rules

Let:

- `S` = current in-memory `last_applied_command_id`
- `R` = current in-memory `run_epoch`
- `W` = current in-memory `worker_id`
- `B` = handshake `draft_base_command_id`
- `R'` = handshake `run_epoch`
- `W'` = handshake `worker_id`

### Case A: exact match

If:

- `W' == W`
- `R' == R`
- `B == S`

then the tail begins immediately after the durable snapshot.

Action:

- flush buffered parts
- forward future live parts
- enter `TailAttached`

### Case B: owner is ahead

If:

- `W' == W`
- `R' == R`
- `B > S`

then the owner is ahead of the durable snapshot.

Action:

- keep the already-sent snapshot
- start buffering relay parts locally
- continue applying committed pubsub deltas to the in-memory snapshot
- wait until the watermark advances to `B`
- then flush the buffered tail and enter `TailAttached`

This is important: **do not refetch the full snapshot immediately** just because
`B > S`.

### Case C: stale or incompatible handshake

If:

- `B < S`, or
- `W' != W`, or
- `R' != R`

then the tail does not line up with the snapshot.

Action:

- discard that handshake
- restart attach logic
- if a gap or timeout is later detected, fall back to resync

## Stream connection state machine

Each connected stream behaves like a small state machine.

### `SnapshotOnly`

The connection has a durable snapshot and no attached live tail.

### `CatchingUpToTailBase`

The connection has:

- already sent the durable snapshot
- a valid handshake for the same owner and run epoch
- `draft_base_command_id > last_applied_command_id`

In this state:

- relay parts are buffered, not forwarded
- committed pubsub deltas advance the in-memory snapshot
- once the watermark reaches `draft_base_command_id`, the buffered tail is
flushed and the connection becomes `TailAttached`

### `TailAttached`

The durable snapshot and live tail are aligned.

In this state:

- committed pubsub deltas continue updating the in-memory snapshot
- relay parts are forwarded directly to the client
- owner / run changes force detach and restart

### `Resyncing`

The connection detected a gap, timeout, mismatch, or buffer overflow.

In this state:

- stop forwarding relay parts
- discard buffered tail
- rebuild from a fresh durable snapshot and handshake

## Delta application rules

For a connected stream, pubsub updates should be applied incrementally to the
in-memory durable snapshot. The delta payload should embed the applied command
result directly rather than forcing listeners to do an extra database lookup.

Let `S` be the connection’s current `last_applied_command_id`.

For a delta carrying `applied_command.id = C`:

- if `C == S + 1`, apply it and advance `S`
- if `C <= S`, ignore it as stale / duplicate
- if `C > S + 1`, a gap was detected and the connection must enter
`Resyncing`

This keeps pubsub out of the correctness path. A missed delta becomes an
observable gap, not silent corruption.

## Relay role

Relay is still required for the live tail.

Correct role of relay:

- a non-owner replica serves the durable committed prefix from Postgres
- then uses `worker_id` to reach the owner
- then receives buffered + future live parts for the current `run_epoch`

Relay is **not** required for durable committed state. It is required only for
the current ephemeral tail.

## Linearization point per applied command

Each applied command should linearize at one DB transaction that:

1. locks the chat snapshot row, or creates it for `CreateChat`
2. verifies the owner lease is still held
3. reads the next pending command for the chat
4. verifies it is the next expected command for the chat
5. applies the transition to `chats`, `chat_messages`, queue projection, and
   `pending_action`
6. advances `last_applied_command_id`
7. appends any follow-up internal commands that become immediately runnable
8. writes the short-lived success or error result to `chat_command_results` if
   needed for external waiters / idempotency
9. deletes the processed command row from `chat_commands`

If the new snapshot state contains runnable work, that transaction should append
an internal `StartRun` command rather than launching the effect inline.

After commit, the owner publishes:

- the corresponding committed-state delta for streams, and
- a small result-ready wake keyed by `command_id` when an external waiter may be
  blocked on `chat_command_results`.

If either publish is delayed or lost, connected streams detect the gap and
resync, while waiting HTTP handlers fall back to re-reading
`chat_command_results`.

## HTTP compatibility layer

Current mutating endpoints should become:

1. enqueue durable command
2. wait for the corresponding `chat_command_results` row
3. reread `chat_command_results` to obtain the stored payload
4. return the same response shape as today

The request waits only for the actor transition, not for full assistant
completion.

### Result-wait notification model

The request-side waiter should use a hybrid mechanism:

- durable truth: `chat_command_results`
- fast wake-up: pubsub result-ready notification
- correctness fallback: periodic DB recheck until timeout / cancellation

Recommended flow:

1. enqueue command and get `command_id`
2. register a local in-memory waiter keyed by `command_id`
3. immediately check `chat_command_results` once to close the fast-completion
   race
4. if not found, wait on:
   - local waiter signal from pubsub, or
   - periodic timer for DB recheck, or
   - request context cancellation
5. when woken, read `chat_command_results` and return the stored payload

Each replica should keep a process-wide subscription for result-ready
notifications. After an owner commits a command result row, it should publish a
small notification keyed by `command_id`. A waiting request on any replica wakes
up, re-reads `chat_command_results`, and returns.

This preserves the main design rule:

- database rows are truth
- pubsub is only a wake-up hint

Special case for `CreateChat`:

- preallocate the chat UUID before enqueueing
- route by that UUID
- insert the chat row using that preallocated ID

## Reference flow: user sends a message to an idle chat

Assume `status = waiting` or `error`, with no backlog, no pending action, and
no active run.

1. Client calls `POST /chats/{id}/messages`.
2. Receiving replica appends `SendMessage` to `chat_commands` and publishes a
  wake with `chat_id`.
3. Replicas race to claim via `worker_id` / `heartbeat_at`; one becomes owner.
4. Owner applies `SendMessage` in one transaction: insert user message, update
  snapshot, advance `last_applied_command_id`, store command result, append
   internal `StartRun`.
5. Waiting HTTP request returns the same direct-send response shape as today.
6. Owner publishes the committed delta.
7. Owner applies `StartRun`, increments `run_epoch`, sets `status=running`, and
  starts `chatloop.Run`.
8. Owner emits ephemeral in-flight `message_part`s locally and via relay.
9. When a durable step/result is ready, the effect appends an internal command.
10. Actor applies it, commits snapshot changes, advances
  `last_applied_command_id`, and publishes the next delta.
11. Repeated self-loop transitions stay on the same owner while the chat is
  hot, and active stream connections pin the same owner for viewer locality.
12. Ownership is released only after the chat is not hot and no active stream
   pin remains.

## Public API compatibility expectations

Preserve these behaviors:

1. `CreateChat` still returns a created chat row immediately.
2. `CreateChatMessage` still returns either `queued=true` plus a queued message,
  or a directly inserted user message, without waiting for assistant
   completion.
3. `EditChatMessage`, `DeleteQueued`, `PromoteQueued`, `InterruptChat`, and
  `SubmitToolResults` keep their current endpoint shapes and broad error
   semantics.
4. queued message IDs remain stable and user-addressable.
5. backlog behavior remains queue-preserving until drained.
6. interrupted runs may still persist one partial assistant/tool suffix.
7. manual non-head queue promotion remains allowed.
8. `StreamChat` remains a snapshot-first stream.
9. `WatchChats` remains owner-scoped and best-effort across chats.

Acceptable additive changes: relay metadata such as `part_seq`, an additive
client event for edit/timeline reset if needed, and observability/debug
endpoints for owner leases and watermark lag. No new public durable stream
cursor such as `after_seq` is required in this revision.

## Package and file reshaping

Expected new or expanded areas:

- `coderd/x/chatd/actor/` for command-queue apply logic and owner-loop runtime
- `coderd/x/chatd/stream/` for snapshot assembly, delta application, handshake
logic, tail buffering, and resync
- `coderd/x/chatd/effects/` for the `chatloop.Run` bridge
- `coderd/database/queries/` additions for command-queue and claim queries
- `coderd/exp_chats.go` for handler enqueue-and-wait integration
- `enterprise/coderd/x/chatd/chatd.go` for relay logic that now serves only the
ephemeral tail

## Implementation structure: three state machines

This redesign should be implemented as three separate state machines with clear
boundaries.

### 1. Core chat state machine

This is the correctness foundation. It owns the durable chat semantics:

- `chat_commands`
- `chat_command_results`
- `run_epoch`
- `last_applied_command_id`
- `pending_action`
- terminal vs retryable error classification
- all durable chat transitions, including queueing, interrupt, edit,
  `requires_action`, archive, and recovery

The core machine should assume one external guarantee:

> next-command application is serialized per chat.

That is, the core state machine does not define ownership policy, but it does
assume that only one executor at a time applies the next command for a given
chat.

### 2. Stream state machine

This is separate from the core machine. It consumes:

- the durable committed snapshot from Postgres
- incremental committed deltas from pubsub
- the owner handshake and relay tail when a run is active

It owns:

- snapshot assembly
- in-memory snapshot updates
- tail attach / catch-up / resync behavior
- client-visible ordering during a connected stream

### 3. Ownership state machine

This is a runtime ownership policy layer on top of the core executor.

It owns:

- owner lease retention
- hot vs quiescent rules
- active `StreamChat` pinning rules
- no-steal behavior when a fresh owner exists
- lease release rules
- heavy working-set residency policy

The core chat machine and the stream machine should not need to know its
internal policy, beyond consuming the external guarantee that command execution
is serialized per chat and that `worker_id` identifies the current owner.

## Implementation order

This section is a guide for the engineer doing the refactor, not a statement
about what intermediate commits are individually mergeable. The intended merge
shape is still "all new state machines implemented, legacy code removed." It is
acceptable during implementation to stub deleted paths with predefined errors as
long as the final result removes the stubs before merge.

### Step 0: delete the legacy implementation skeleton first

At the start of the refactor, remove the old chatd control-flow skeleton so the
new design is implemented against a clean target rather than layered on top of
old execution paths.

This includes removing or stubbing out:

- `AcquireChats`-driven execution
- old per-chat heartbeat ownership logic
- control-pubsub cancellation as a correctness mechanism
- direct SQL mutation paths that bypass command application
- old stream merge logic that relied on ad hoc local/pubsub ordering

### Step 1: encode the correctness model as tests

Add deterministic tests for:

- queue-first interrupt semantics
- no-loss queue promotion
- exact `requires_action` closure behavior
- stale callback rejection
- snapshot-first stream ordering
- command ordering under concurrent HTTP requests

### Step 2: implement the full core chat state machine

Implement the full durable chat semantics, not a reduced subset. This includes:

- command queue and result store
- core command application
- queue transitions
- interrupt behavior
- edit behavior
- `requires_action`
- archive / unarchive
- stale recovery
- command deletion / result-row writing
- terminal vs retryable failure handling, where a dedicated terminal-error type
  marks non-retryable failures and all other execution failures are retried up
  to the configured cap

### Step 3: implement the stream state machine

Implement the stream behavior as a separate machine that consumes the core
machine's durable snapshot and deltas. This includes:

- consistent initial snapshot read
- incremental pubsub delta application
- owner handshake
- tail attach / catch-up / resync behavior

### Step 4: implement the ownership state machine

Implement the runtime ownership optimization layer. This includes:

- sticky owner sessions
- active `StreamChat` pinning
- no-steal behavior for fresh owners
- ownership release after quiescence
- separation of ownership pinning from heavy working-set residency

### Step 5: finalize and remove temporary stubs

Before merge, remove any predefined-error scaffolding introduced during Step 0
and ensure the three new state machines are the only implementation left.
## TDD execution slices

### Slice A: command queue, claim race, synchronous apply

#### Red

Add failing tests that prove:

- commands are applied once, in queue order
- no two replicas apply commands concurrently for the same chat
- handler retries with the same idempotency key reuse one stored result
- if multiple replicas race to claim a chat, only one continues
- a hot chat remains pinned to one owner across repeated self-loop transitions
- an active `StreamChat` connection pins the current owner instead of moving the
  chat to the connection-serving replica
- later stream connections on other replicas do not steal ownership from a fresh
  owner

#### Green

Implement:

- command schema
- claim / release / heartbeat queries
- wake listener
- owner loop
- stored command results
- handler wait logic
- sticky owner-session retention logic
- stream-pin retention logic for active `StreamChat` connections
- no-steal behavior for later stream connections when a fresh owner already
  exists

#### Refactor

Extract focused actor, ownership, and wait-path packages.

### Slice B: create and idle send through completed run

#### Red

Add failing tests that prove:

- preallocated-ID `CreateChat` preserves current HTTP behavior
- idle `SendMessage` preserves current HTTP behavior
- an enqueued idle `SendMessage` starts processing immediately via wake + claim
- stale callbacks from run N do not affect run N+1
- repeated `CommitStep` self-loops stay on one owner without full DB history
reload each turn

#### Green

Implement:

- actor-backed `CreateChat`
- actor-backed idle `SendMessage`
- `StartRun`
- effect-to-command-queue callbacks
- committed delta publication for committed transitions
- owner working-set updates across repeated self-loops

#### Refactor

Move direct persistence and committed status publication behind actor helpers.

### Slice C: `requires_action`, interrupt, and recovery

#### Red

Add failing tests that prove:

- `requires_action` is driven by explicit `pending_action`
- `SubmitToolResults` resumes only the current `run_epoch`
- stale recovery and interrupt emit synthetic tool results exactly once
- late callbacks after interrupt / recovery do not mutate durable state

#### Green

Implement:

- authoritative `pending_action`
- actor-backed `SubmitToolResults`
- actor-backed `Interrupt`
- actor-backed recovery commands
- `run_epoch` fencing everywhere

#### Refactor

Delete history-derived pending-call logic from migrated paths.

### Slice D: stream stitching and incremental durable updates

#### Red

Add failing tests that prove:

- a stream connection reads a consistent initial durable snapshot
- if `draft_base_command_id == last_applied_command_id`, the tail attaches
immediately
- if `draft_base_command_id > last_applied_command_id`, the stream sends the
snapshot immediately, buffers the tail, waits for pubsub catch-up, then
attaches
- if `draft_base_command_id < last_applied_command_id`, the handshake is
rejected and attach logic restarts
- connected streams apply deltas in memory instead of refetching the whole
snapshot on every commit
- pubsub gaps trigger resync
- owner or run-epoch changes during catch-up discard the buffered tail and
restart attach
- stale relayed parts from an old `run_epoch` are discarded

#### Green

Implement:

- stream connection state machine
- owner handshake payload
- tail buffering
- incremental delta application
- gap detection and resync

#### Refactor

Delete old stream merge code that relied on ad hoc local/pubsub ordering.

### Slice E: queue, edit, archive, and stream reset behavior

#### Red

Add failing tests that prove:

- queue promotion is atomic and lossless
- edit clears queue and pending action in one transition
- edit/reset behavior is represented correctly to connected streams
- archive/unarchive do not break owner or stream invariants

#### Green

Implement:

- actor-backed queue admission and promotion
- actor-backed edit
- actor-backed archive/unarchive
- stream reset handling for edit/truncation

#### Refactor

Delete cleanup-time queue mutation helpers and remaining direct mutation paths.

## Verification strategy

1. Unit tests exercise actor apply functions directly.
2. Integration tests verify HTTP compatibility and stream stitching behavior.
3. Multi-replica tests verify claim races, failover, sticky owner sessions, and
  stale callback rejection.
4. Enterprise tests verify relay is used only for the live tail, not for
  durable committed state.
5. The chat integration suite is updated to validate the refactored implementation directly.
6. Manual verification covers idle send, running send, interrupt, edit,
  tool submission, reconnect on owner and non-owner replicas, owner crash /
   failover, relay present vs absent, and pubsub-gap resync.

## Risks and mitigations

- **Wake fan-out causes contention.** Keep wake pubsub as a latency hint only;
claim race is the real arbiter.
- **`worker_id` semantic shift breaks assumptions elsewhere.** Audit all uses of
`worker_id` and update tests to reflect its new meaning.
- **Ephemeral tail is lost on owner crash.** Accept that raw in-flight parts are
ephemeral; rebuild from durable committed state plus pending command-queue state and fence
late callbacks with `run_epoch`.
- **Pubsub gaps or reordering break connected streams.** Anchor every stream to
`last_applied_command_id`, apply only contiguous deltas, and force resync on
gaps.
- **Owner stays ahead of the snapshot for too long.** Bound
`CatchingUpToTailBase` buffering and timeout; overflow or stall triggers
resync.

## Exit criteria

The redesign is complete only when:

1. every chat mutation enters through `chat_commands`
2. one chat owner at a time serializes command-queue execution through the DB lease
3. every long-running callback is fenced by `run_epoch`
4. connected streams maintain durable correctness through
  `last_applied_command_id` plus gap-detecting delta application
5. hot chats stay pinned to one owner session until quiescent instead of
  bouncing across replicas on each self-loop
6. active `StreamChat` connections pin the current owner instead of causing
  ownership flapping across replicas
7. non-owner replicas can serve the durable committed prefix of a running chat
  without talking to the owner
8. relay is used only for the active ephemeral tail of a running chat
9. legacy coordination code can be deleted without reintroducing the known race
  classes

## Exhaustive TODO

- Confirm that the target architecture is a PostgreSQL-backed command queue plus per-chat owner lease model, not partition hashing and not a full event-sourced rewrite.
- Turn the invariants from `chatd-loop-correctness-model.md` into deterministic tests.
- Add regression tests for the current hotspots in `SendMessage`, `processChat`, `tryAutoPromoteQueuedMessage`, `SubmitToolResults`, `InterruptChat`, `recoverStaleChats`, and `Subscribe`.
- Decide the exact external command taxonomy and JSON payload schema.
- Decide the exact internal effect-command taxonomy and payload schema.
- Decide the exact schema and TTL policy for `chat_command_results`.
- Lock the exact committed-state delta schema carried over pubsub, including the embedded `applied_command` payload.
- Decide whether commands with no visible client effect still publish an explicit watermark delta.
- Decide the bounded buffer policy for `CatchingUpToTailBase`.
- Decide the timeout policy for handshake catch-up and resync.
- Decide whether `pending_action` stays as JSONB on `chats` or moves to a dedicated table after the first cut.
- Add migration(s) for `chat_commands`.
- Add migration(s) for `chat_command_results`.
- Remove any planned migration(s) for a durable per-chat event-log table.
- Add `run_epoch` to `chats`.
- Add `last_applied_command_id` to `chats`.
- Add `pending_action` to `chats`.
- Add any additional owner-lease metadata needed on `chats`.
- Add SQL queries to enqueue commands.
- Add SQL queries to fetch the next pending command in queue order.
- Add SQL queries to delete a processed command in the apply transaction.
- Add SQL queries to try-claim chat ownership via `worker_id` / `heartbeat_at`.
- Add SQL queries to release chat ownership.
- Add SQL queries to heartbeat chat ownership.
- Add SQL queries to store and read `chat_command_results` payloads.
- Add TTL cleanup for expired `chat_command_results` rows.
- Run `make -j16 gen` after query changes.
- Update audit handling if any new auditable fields require it.
- Implement a wake channel carrying `chat_id`.
- Implement owner claim-race logic on wake.
- Implement periodic polling fallback for missed wakes.
- Implement per-chat owner loops.
- Implement owner heartbeats while active work exists.
- Implement one-active-transition-per-chat execution.
- Define hot vs quiescent owner-session rules explicitly in code and tests.
- Keep hot chats pinned to one owner session across repeated self-loop
transitions.
- Release ownership only after the chat becomes quiescent.
- Implement `enqueue + wait for result` for synchronous HTTP handlers.
- Add idempotency handling for handler retries and internal retries.
- Preallocate chat IDs in the HTTP layer before `CreateChat` command enqueue.
- Implement the first no-op or metadata-only command to prove the runtime.
- Migrate `CreateChat` to actor mode.
- Migrate idle `SendMessage` to actor mode.
- Add `StartRun` as an explicit actor transition.
- Make `runChat` start through the actor instead of `AcquireChats`.
- Add `run_epoch` to every long-running effect callback path.
- Make stale effect callbacks no-op by `run_epoch` comparison.
- Route assistant/tool persistence through actor-owned transitions.
- Route terminal success and error transitions through actor-owned transitions.
- Introduce a dedicated terminal-error type; treat that type as non-retryable and all other execution failures as retryable up to the configured cap.
- Publish committed-state deltas after every applied command.
- Delete successfully or terminally processed commands immediately after apply.
- Leave retryable commands pending, with retry/backoff metadata, instead of deleting them early.
- Treat successful processing, terminal failure, and retryable failure as distinct command outcomes throughout code and tests.
- Ensure connected stream handlers can apply deltas in-memory without full snapshot refetch on every commit.
- Maintain an in-memory working set on the owner so repeated self-loop turns
do not rebuild full history from the database.
- Keep relay attachments stable across repeated self-loop turns on one
owner.
- Keep `after_id` compatibility for the current snapshot-first stream endpoint.
- Migrate `requires_action` entry to actor mode.
- Replace history-derived pending-call inference with authoritative `pending_action` on migrated paths.
- Migrate `SubmitToolResults` to actor mode.
- Migrate `Interrupt` to actor mode.
- Migrate stale running recovery to command-driven actor recovery.
- Migrate stale `requires_action` recovery to command-driven actor recovery.
- Ensure synthetic tool-result closure is actor-owned and exactly once.
- Migrate busy-send queue admission to actor mode.
- Keep `chat_queued_messages` as an actor-owned projection for API compatibility.
- Migrate manual queued-message delete to actor mode.
- Migrate manual queued-message promote to actor mode.
- Migrate auto-promote head to an explicit actor transition.
- Migrate edit truncation and restart to actor mode.
- Migrate archive to actor mode.
- Migrate unarchive to actor mode.
- Define the owner handshake payload with `worker_id`, `run_epoch`, `draft_base_command_id`, `tail_open`, `next_part_seq`, buffered parts, and `part_seq`.
- Implement stream setup as subscribe-first, then consistent snapshot read.
- Implement per-connection in-memory snapshot state.
- Implement `SnapshotOnly`, `CatchingUpToTailBase`, `TailAttached`, and `Resyncing` stream states.
- Implement the exact-match tail attach path.
- Implement the owner-ahead buffering path.
- Implement stale/incompatible handshake rejection.
- Send the durable snapshot immediately even when owner-ahead buffering is in progress.
- Buffer relay parts until the in-memory snapshot watermark reaches `draft_base_command_id`.
- Flush buffered parts only after the durable watermark catches up.
- Tag ephemeral parts with `chat_id`, `run_epoch`, and `part_seq`.
- Discard stale relayed parts from old `run_epoch`s.
- Apply contiguous deltas in-memory when `applied_command.id == last_applied_command_id + 1`.
- Ignore stale/duplicate deltas when `applied_command.id <= last_applied_command_id`.
- Trigger resync when `applied_command.id > last_applied_command_id + 1`.
- Trigger resync on tail buffer overflow or timeout.
- Restart tail attach when owner or run epoch changes during catch-up.
- Make `WatchChats` publish only actor-committed lifecycle state.
- Audit all uses of `worker_id` and update assumptions to the new owner lease meaning.
- Add multi-replica claim-race tests.
- Add stale callback rejection tests for run-epoch fencing.
- Add stream tests for exact-match tail attach.
- Add stream tests for owner-ahead tail buffering.
- Add stream tests for pubsub-gap resync.
- Add stream tests for owner/run changes during catch-up.
- Add tests that a non-owner replica can serve the durable prefix of a running chat.
- Add API compatibility tests for create, send, edit, queue, interrupt, and tool-result endpoints.
- Add enterprise tests proving relay is required only for the active tail, not for durable committed ordering.
- Add metrics for command-queue depth, command-apply lag, claim contention, watermark lag, stale callback drops, and processed-command deletion rate.
- Add metrics for handshake mismatch, catch-up buffering, and resync rate.
- Add metrics for hot owner-session duration, lease churn, and relay reconnect churn.
- Add debug visibility for per-chat command history, owner lease, stream watermark, and attach state.
- Run the chat integration suite against the refactored implementation directly.
- Remove `AcquireChats`-based execution once actor mode is complete.
- Remove the old per-chat heartbeat ownership logic once actor mode is complete.
- Remove control-pubsub cancellation from the correctness path.
- Remove direct SQL mutation helpers that bypass actor apply for migrated command families.
- Remove old stream merge code once snapshot + delta + tail serving is complete.
- Re-run the full correctness test suite after deleting legacy code.
- Do not start implementation until this plan is approved.

