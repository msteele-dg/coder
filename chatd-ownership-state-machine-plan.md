# chatd ownership state machine plan

## Status

Draft for review only. Do not start implementation until this plan is explicitly approved.

## Scope

This file covers the **ownership state machine only**.

Its job is to control **ownership lifetime and locality**, not durable chat
semantics and not stream stitching.

It decides:

- when a chat remains pinned to one owner,
- when active viewers pin the owner,
- when ownership may be released,
- when heavy working-set residency may be reduced.

It does **not** own:

- durable command semantics,
- queue / edit / interrupt / recovery transitions,
- snapshot assembly,
- relay tail attach protocol.

Those belong to the other two state machines.

## Problem

In real-world use, a chat spends most of its time looping on repeated
`CommitStep`-style self-transitions before eventually reaching `FinishWaiting`.

A naive ownership model would allow a different replica to execute each turn.
That would cause:

- full prompt/history reconstruction from the database on every self-loop turn,
- relay reconnect churn on every self-loop turn,
- active viewers bouncing across replicas unnecessarily.

The ownership state machine exists to preserve locality while the chat is still
hot or while active viewers are attached.

## Core principle

The core chat machine owns **semantics**.
The stream machine owns **connected ordering / attach behavior**.
The ownership state machine owns **ownership lifetime**.

The ownership state machine should be an optimization layer that provides this external
runtime guarantee:

> command execution remains serialized per chat, and ownership is retained while
> the chat is hot or actively pinned.

## Inputs

The ownership state machine consumes:

- current owner lease state (`worker_id`, `heartbeat_at`),
- whether a run is active,
- whether immediately runnable internal commands exist,
- whether queue follow-up can run immediately,
- whether a live tail is active or buffered,
- whether active `StreamChat` pins exist.

## States

### `Unowned`

No fresh owner lease exists for the chat.

### `OwnedHot`

The chat has a fresh owner and is hot because active or immediately runnable
work exists.

### `OwnedPinned`

The chat has a fresh owner and is not necessarily hot, but at least one active
`StreamChat` connection pins the current owner.

### `OwnedHotAndPinned`

The chat is both hot and pinned.

### `Releasable`

The chat still has an owner, but no hot work and no stream pin remain. It may
release ownership.

## Hot rules

A chat is **hot** while any of these are true:

- an LLM run is active,
- immediately runnable internal commands exist,
- queue follow-up can be executed immediately,
- an ephemeral live tail is active or buffered,
- there is already known local work that the same owner can continue without a
  new global claim cycle.

## Pin rules

A chat is **pinned** while at least one active `StreamChat` connection exists
for that chat.

Important policy:

- if the chat is unowned, the replica serving the first active stream
  connection may claim it,
- if the chat already has a fresh owner, later stream connections pin the
  existing owner instead of moving the chat,
- a new stream connection on another replica must not steal ownership from a
  fresh owner.

## Lease retention rules

Do not release the owner lease after every transition.

Keep ownership while the chat is:

- `OwnedHot`, or
- `OwnedPinned`, or
- `OwnedHotAndPinned`.

Release ownership only when all are true:

- the active run has stopped,
- no immediately runnable internal command remains,
- no immediate queue follow-up remains,
- no buffered / active live tail still needs serving,
- no active `StreamChat` pin remains.

## Local fast path

While a chat is hot, the same owner should continue draining the durable command
queue locally.

Important constraint:

- internal follow-up commands must still be durably enqueued,
- the same owner may consume them without a new global claim cycle,
- but it must still consume the **next pending command in durable queue order**.

This preserves correctness even if another replica enqueues an external command
between two self-loop turns.

## Memory / residency rule

Ownership pinning and heavy working-set residency should be treated separately.

- keep the owner lease pinned while stream pins exist,
- keep the heavy prompt/history/runtime working set resident only while the chat
  is hot or tail buffering is active,
- allow the heavy working set to be evicted when the chat is idle-but-pinned.

This preserves viewer locality without forcing maximum memory residency forever.

## Relationship to the core chat machine

The ownership state machine must not redefine chat semantics.

The core machine still decides:

- which command is next,
- what state transition is valid,
- what durable snapshot mutation occurs.

The ownership state machine only decides whether the same owner should continue holding execution locality.

## Relationship to the stream machine

The ownership state machine does not define attach order or stream reconciliation.

It only provides ownership stability so that:

- active viewers do not needlessly switch replicas,
- relay attachments can remain stable across repeated self-loop turns.

## Implementation order for this machine

1. define explicit hot vs quiescent rules in code,
2. define explicit stream-pin rules,
3. keep ownership across repeated self-loop transitions,
4. prevent later stream connections from stealing ownership from a fresh owner,
5. release ownership only after quiescence and pin release,
6. separate owner pinning from heavy working-set residency.

## Verification

At minimum, tests for this machine should prove:

- a hot chat remains pinned to one owner across repeated self-loop transitions,
- an active `StreamChat` connection pins the current owner,
- later stream connections on other replicas do not steal a fresh owner,
- ownership is released only after the chat is not hot and no pin remains,
- relay attachments can remain stable while a chat stays on one owner,
- idle-but-pinned chats may evict heavy working state without releasing
  ownership.

## Exhaustive TODO

- [ ] Define hot vs quiescent owner-session rules explicitly in code and tests.
- [ ] Keep hot chats pinned to one owner session across repeated self-loop transitions.
- [ ] Pin the current owner while active `StreamChat` connections exist.
- [ ] If a chat is unowned, allow the first active stream connection to claim it.
- [ ] Prevent later stream connections on other replicas from stealing ownership from a fresh owner.
- [ ] Release ownership only after the chat is not hot and no active stream pin remains.
- [ ] Separate owner pinning from heavy working-set residency so idle-but-pinned chats can evict prompt/history caches.
- [ ] Maintain an in-memory working set on the owner so repeated self-loop turns do not rebuild full history from the database.
- [ ] Keep relay attachments stable across repeated self-loop turns on one owner.
- [ ] Keep relay attachments stable while active stream pins hold ownership.
- [ ] Add tests for sticky owner retention across self-loop turns.
- [ ] Add tests for active stream-pin retention.
- [ ] Add tests for no-steal behavior when a fresh owner already exists.
- [ ] Add tests for lease release only after quiescence and pin release.
- [ ] Add tests for working-set eviction while ownership remains pinned.
- [ ] Add metrics for hot owner-session duration, lease churn, relay reconnect churn, and stream-pin duration.
