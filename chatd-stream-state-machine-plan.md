# chatd stream state machine plan

## Status

Draft for review only. Do not start implementation until this plan is explicitly approved.

## Scope

This file covers the **stream state machine only**.

Its job is to make the current `StreamChat` endpoint work correctly in the new
architecture without requiring a durable per-chat event-log table.

The stream machine consumes:

- the durable committed snapshot from Postgres,
- committed pubsub deltas emitted by the core chat machine,
- the owner handshake and relay tail while a run is active.

It does **not** own:

- durable chat semantics,
- queue / edit / interrupt / recovery state transitions,
- ownership lifetime and pinning policy.

Those belong to the other two state machines.

## Problem

A connected chat stream must present one coherent ordered view built from two
sources:

- the **durable committed prefix** from Postgres,
- the **ephemeral live tail** from the current owner replica.

The hard part is stitching them together without races.

## Inputs

### Durable inputs

- `chats.status`
- `chats.worker_id`
- `chats.run_epoch`
- `chats.last_applied_command_id`
- committed `chat_messages`
- committed `chat_queued_messages`
- committed `pending_action`
- committed pubsub deltas, each carrying an embedded `applied_command`

### Ephemeral inputs

From the owner handshake / relay tail:

- `worker_id`
- `run_epoch`
- `draft_base_command_id`
- `tail_open`
- `next_part_seq`
- `buffered_parts`
- future live parts

## Durable anchor

The stream machine is anchored by:

- `chats.last_applied_command_id`

This is the durable answer to:

> which committed state changes are included in the snapshot I just read?

All connected-stream ordering is built around that watermark.

## Stream setup contract

The stream stays **snapshot-first**.

### Setup sequence

1. open websocket,
2. subscribe to chat pubsub,
3. read one consistent durable snapshot from Postgres,
4. send the durable snapshot to the client immediately,
5. if the snapshot says `status != running`, enter `SnapshotOnly`,
6. if the snapshot says `status == running`, perform owner handshake and apply
   the tail-attach rules below.

The initial snapshot must be read as one consistent DB snapshot so the content
and watermark correspond to the same committed state.

## Owner handshake contract

When the snapshot says the chat is running, the serving replica asks the owner
for a captured live-tail snapshot.

Required handshake payload:

- `worker_id`
- `run_epoch`
- `draft_base_command_id`
- `tail_open`
- `next_part_seq`
- `buffered_parts`

`draft_base_command_id` means:

> the current in-flight draft begins immediately after durable committed state
> version N.

The owner must capture the handshake fields and buffered parts under one
consistent in-memory read so they all describe the same tail.

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

- `tail_open = true`
- `W' == W`
- `R' == R`
- `B == S`

then the tail begins immediately after the durable snapshot.

Action:

- send buffered parts,
- forward future live parts,
- enter `TailAttached`.

### Case B: owner is ahead

If:

- `tail_open = true`
- `W' == W`
- `R' == R`
- `B > S`

then the owner is ahead of the durable snapshot.

Action:

- keep the already-sent durable snapshot,
- buffer relay parts locally,
- continue applying committed pubsub deltas to the in-memory snapshot,
- wait until the watermark advances to `B`,
- then flush the buffered tail and enter `TailAttached`.

Do **not** immediately refetch the full snapshot just because `B > S`.

### Case C: stale or incompatible handshake

If:

- `B < S`, or
- `W' != W`, or
- `R' != R`

then the tail does not line up with the snapshot.

Action:

- reject the handshake,
- discard any buffered tail from that handshake,
- restart attach logic,
- if gaps or repeated mismatch are detected, enter `Resyncing`.

### Case D: no open tail

If:

- `tail_open = false`

then there is no attachable live draft.

Action:

- remain `SnapshotOnly`.

## Stream runtime states

### `SnapshotOnly`

The connection has a durable snapshot and no attached live tail.

### `CatchingUpToTailBase`

The connection has:

- already sent the durable snapshot,
- a valid owner handshake for the same owner and run epoch,
- `draft_base_command_id > last_applied_command_id`.

In this state:

- relay parts are buffered, not forwarded,
- committed deltas advance the in-memory snapshot,
- once the watermark reaches `draft_base_command_id`, the buffered tail is
  flushed and the connection becomes `TailAttached`.

### `TailAttached`

The durable snapshot and live tail are aligned.

In this state:

- committed deltas continue updating the in-memory snapshot,
- relay parts are forwarded directly to the client,
- owner / run changes force detach and restart.

### `Resyncing`

The connection detected a gap, timeout, mismatch, or buffer overflow.

In this state:

- stop forwarding relay parts,
- discard buffered tail,
- rebuild from a fresh durable snapshot and handshake.

## Delta application rules

Connected streams apply committed pubsub deltas incrementally to the in-memory
snapshot.

Let `S` be the connection’s current `last_applied_command_id`.

For a delta carrying `applied_command.id = C`:

- if `C == S + 1`, apply it and advance `S`,
- if `C <= S`, ignore it as stale or duplicate,
- if `C > S + 1`, a gap was detected and the connection must enter
  `Resyncing`.

The delta payload should embed the applied command result directly rather than
forcing listeners to do an extra database lookup.

## Relay role

Relay is required only for the active ephemeral tail.

A non-owner replica should:

- serve the durable committed prefix from Postgres,
- use `worker_id` to reach the owner,
- receive buffered and future live parts for the current `run_epoch`.

Relay is **not** the source of truth for durable committed state.

## Relationship to the core chat machine

The stream machine depends on the core machine to provide:

- consistent durable snapshots,
- `last_applied_command_id`,
- `run_epoch`,
- committed pubsub deltas with embedded `applied_command`,
- owner identity in `worker_id`.

It must not reimplement or reinterpret durable chat semantics.

## Relationship to the ownership state machine

The stream machine should not own pinning policy.

It only consumes:

- the current owner identity,
- the fact that ownership may remain stable while viewers are connected.

## Implementation order for this machine

1. implement consistent initial snapshot read,
2. implement per-connection in-memory snapshot state,
3. implement committed delta application,
4. implement owner handshake,
5. implement tail attach rules,
6. implement catch-up buffering,
7. implement gap detection and resync,
8. remove the old ad hoc stream merge behavior.

## Verification

At minimum, tests for this machine should prove:

- the initial snapshot is consistent,
- exact-match handshake attaches the tail immediately,
- owner-ahead handshake buffers tail until the snapshot catches up,
- stale / incompatible handshake is rejected,
- connected streams apply incremental deltas without refetching the whole
  snapshot on every commit,
- pubsub gaps force resync,
- owner or run changes during catch-up discard the buffered tail and restart,
- stale relayed parts from an old `run_epoch` are discarded.

## Exhaustive TODO

- [ ] Implement stream setup as subscribe-first, then consistent snapshot read.
- [ ] Read the initial snapshot from one consistent DB snapshot.
- [ ] Implement per-connection in-memory snapshot state.
- [ ] Define and implement the owner handshake payload with `worker_id`, `run_epoch`, `draft_base_command_id`, `tail_open`, `next_part_seq`, buffered parts, and `part_seq`.
- [ ] Implement exact-match tail attach.
- [ ] Implement the owner-ahead buffering path.
- [ ] Implement stale / incompatible handshake rejection.
- [ ] Send the durable snapshot immediately even when owner-ahead buffering is in progress.
- [ ] Buffer relay parts until the in-memory watermark reaches `draft_base_command_id`.
- [ ] Flush buffered parts only after the durable watermark catches up.
- [ ] Tag ephemeral parts with `chat_id`, `run_epoch`, and `part_seq`.
- [ ] Discard stale relayed parts from old `run_epoch`s.
- [ ] Apply contiguous deltas in memory when `applied_command.id == last_applied_command_id + 1`.
- [ ] Ignore stale / duplicate deltas when `applied_command.id <= last_applied_command_id`.
- [ ] Trigger resync when `applied_command.id > last_applied_command_id + 1`.
- [ ] Trigger resync on tail buffer overflow or timeout.
- [ ] Restart tail attach when owner or run epoch changes during catch-up.
- [ ] Add tests for exact-match tail attach.
- [ ] Add tests for owner-ahead tail buffering.
- [ ] Add tests for stale / incompatible handshake rejection.
- [ ] Add tests for pubsub-gap resync.
- [ ] Add tests for owner/run changes during catch-up.
- [ ] Add tests that a non-owner replica can serve the durable prefix of a running chat.
- [ ] Remove old stream merge code once snapshot + delta + tail serving is complete.
