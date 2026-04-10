# chatd loop correctness model

## Scope

This document models the **durable one-chat semantics** of `chatd`.

Included:

- chat history
- queued messages
- status transitions
- worker ownership
- dynamic-tool pause/resume
- interrupt / edit / recovery

Explicitly excluded from this model:

- streaming token delivery
- relay / multi-replica transport
- frontend rendering
- pubsub delivery mechanics
- title generation
- git sync
- push notifications
- interactions between multiple chats

This model treats **active history** as the logical projection of
`chat_messages` after soft-deletes are applied. In other words, `EditMessage`
is modeled as suffix truncation plus replacement, not as raw row-level mutation
mechanics.

## Semantic choices fixed for this model

### 1. Best-effort interrupt

`busy_interrupt` is **not atomic**. It is modeled as:

1. admit the new user message into the queue; then
2. make a best-effort request to stop the currently running loop.

So the guarantee is:

- the new message is preserved and queued;
- the old run is asked to stop;
- the old run may still make some additional progress before the interrupt
  takes effect.

This means the system is modeled as **"queue, then try to interrupt"**, not
**"atomically replace the current run with the new one"**.

### 2. Interrupted partial assistant output is part of the spec

Interrupted runs are allowed to durably persist partial assistant/tool output.
That behavior is part of the intended semantics, not an implementation detail.

## 1. Abstract one-chat specification

### State

```text
State = {
  H: Seq[Msg],                   // committed active history
  Q: Seq[QueuedUserMsg],         // queued user messages
  status: {waiting, pending, running, requires_action, error},
  worker: WorkerID | None,       // owner iff status = running
  pendingCalls: Map[CallID, DynamicCall],
  archived: Bool
}
```

### Message model

```text
Msg =
  | System(parts)
  | User(uid, parts)
  | Assistant(uid, parts)
  | Tool(uid, toolCallID, toolName, result)
```

Notes:

- `uid` is a logical identity used for no-loss / no-dup reasoning.
- `H` is in durable commit order.
- `Q` is ordered by enqueue order.
- `pendingCalls` is the durable set of dynamic tool calls still awaiting
  client-supplied results.

## 2. Abstract transitions

The system is modeled using three classes of transitions:

- external transitions
- worker transitions
- recovery transitions

### External transitions

#### `Create(initialUser)`

Matches: `CreateChat`

Preconditions:

- chat does not yet exist

Effects:

- `H := [deployment/system prompts..., User(initialUser)]`
- `Q := []`
- `status := pending`
- `worker := None`
- `pendingCalls := {}`

#### `DirectSend(m)`

Matches: `SendMessage` when chat is idle.

Preconditions:

- `status ∈ {waiting, error}`
- `Q = []`

Effects:

- `H := H ⧺ [User(m)]`
- `status := pending`
- `worker := None`

#### `Enqueue(m)`

Matches: `SendMessage` when busy or backlog exists.

Preconditions:

- `status ∈ {pending, running, requires_action}`
  or
- `Q ≠ []`

Effects:

- `Q := Q ⧺ [m]`
- all other fields unchanged

This captures the current queue-preserving rule: once backlog exists, even a
later `waiting` state continues queue admission instead of inserting directly
into history.

#### `ManualPromote(qid)`

Matches: `PromoteQueued`

Preconditions:

- `qid ∈ Q`

Effects:

- remove `qid` from `Q`
- `H := H ⧺ [User(qid.content)]`
- `status := pending`
- `worker := None`

Important note: manual promotion may remove a non-head queued item. Therefore
manual promotion is not globally FIFO.

#### `DeleteQueued(qid)`

Matches: `DeleteQueued`

Preconditions:

- `qid ∈ Q`

Effects:

- remove `qid` from `Q`
- everything else unchanged

#### `Edit(k, replacement)`

Matches: `EditMessage`

Preconditions:

- `H[k]` is a user message

Effects:

- `H := prefix(H, k-1) ⧺ [User(replacement)]`
- `Q := []`
- `pendingCalls := {}`
- `status := pending`
- `worker := None`

This models the intended semantics of edit as a logical restart from the edited
user turn.

#### `SubmitToolResults(results)`

Matches: `SubmitToolResults`

Preconditions:

- `status = requires_action`
- submitted result IDs match `pendingCalls` exactly

Effects:

- append one `Tool(...)` message per submitted result to `H`
- `pendingCalls := {}`
- `status := pending`
- `worker := None`

#### `InterruptRunning(optionalPartialStep)`

Matches: `InterruptChat` against a running chat.

Preconditions:

- `status = running`

Effects:

- may append a truncated assistant/tool suffix to `H`
- `status := waiting`
- `worker := None`

This explicitly includes partial-step persistence as part of the abstract
semantics.

#### `InterruptRequiresAction(reason)`

Matches: `InterruptChat` against `requires_action`.

Preconditions:

- `status = requires_action`

Effects:

- append synthetic error tool results for every element of `pendingCalls`
- `pendingCalls := {}`
- `status := waiting`
- `worker := None`

### Worker transitions

#### `Acquire(w)`

Matches: `AcquireChats`

Preconditions:

- `status = pending`
- `worker = None`

Effects:

- `status := running`
- `worker := w`

#### `CommitStep(step)`

Matches: one successful `persistStep(...)` commit.

Preconditions:

- `status = running`

Effects:

- append one durable assistant/tool suffix to `H`

This is where assistant and built-in tool output becomes committed history.
Interrupted partial-step persistence is also allowed to manifest here.

#### `EnterRequiresAction(calls)`

Matches: dynamic-tool path after a step.

Preconditions:

- `status = running`

Effects:

- `pendingCalls := calls`
- `status := requires_action`
- `worker := None`

#### `FinishWaiting`

Matches: process completion with no queue follow-up.

Preconditions:

- `status = running`
- current run has no more work
- `pendingCalls = {}`
- `Q = []`

Effects:

- `status := waiting`
- `worker := None`

#### `AutoPromoteHead`

Matches: cleanup auto-promotion path.

Preconditions:

- `status = waiting`
- `Q ≠ []`
- `archived = false`

Effects:

- let `head(Q) = q`
- `Q := tail(Q)`
- `H := H ⧺ [User(q.content)]`
- `status := pending`
- `worker := None`

This is the FIFO queue-drain transition.

#### `FinishError(err)`

Matches: processing failure path.

Preconditions:

- `status = running`

Effects:

- `status := error`
- `worker := None`

### Recovery transitions

#### `RecoverStaleRunning`

Matches: stale recovery for `running`.

Preconditions:

- `status = running`
- chat is stale

Effects:

- `status := pending`
- `worker := None`

#### `RecoverStaleRequiresAction(reason)`

Matches: stale recovery for `requires_action`.

Preconditions:

- `status = requires_action`
- chat is stale

Effects:

- append synthetic error tool results for every element of `pendingCalls`
- `pendingCalls := {}`
- `status := error`
- `worker := None`

## 3. Verification cut points / linearization points

Invariants are checked only at **stable cut points**, not at arbitrary lines of
code.

### Stable cut points

#### `CP1` — after a public mutation transaction commits

Applies to:

- `CreateChat`
- `SendMessage`
- `EditMessage`
- `PromoteQueued`
- `DeleteQueued`
- `SubmitToolResults`
- `setChatWaiting` / interrupt path

#### `CP2` — after `AcquireChats` commits

This is when worker ownership becomes real.

#### `CP3` — after one `PersistStep` transaction commits

This is when a durable assistant/tool suffix becomes real history.

#### `CP4` — after `processChat` cleanup transaction commits

This finalizes:

- final status
- worker release
- auto-promotion
- queue mutation caused by auto-promotion

#### `CP5` — after stale recovery transaction commits

### Linearization points by operation

#### Operations modeled as atomic

These can be treated as one abstract transition:

- `CreateChat`
- `DirectSend`
- `Enqueue`
- `Edit`
- `ManualPromote`
- `DeleteQueued`
- `SubmitToolResults`
- `Acquire`
- `CommitStep`
- `RecoverStaleRunning`
- `RecoverStaleRequiresAction`

#### Operations modeled as composite

These are not treated as one atomic transition in the current best-effort
spec.

##### `SendMessage` with `busy_interrupt`

Model as two transitions:

1. `Enqueue(m)`
2. best-effort `InterruptRunning(...)` request

The first is guaranteed by success of the send. The second may succeed later,
be delayed, or fail.

##### `InterruptChat` while `status = requires_action`

Model as two logical effects:

1. synthetic tool-result closure
2. transition to `waiting`

##### Run completion with queue follow-up

Concrete code combines status release and queue promotion in cleanup. The model
may view this either as:

- one combined cleanup transition at `CP4`, or
- `FinishWaiting` followed by `AutoPromoteHead`

For implementation reasoning, `CP4` is the more convenient view.

## 4. Safety invariants

These invariants are intended to hold at `CP1..CP5`.

### `I1` Ownership invariant

```text
status = running    iff    worker ≠ None
status ≠ running    iff    worker = None
```

For a single chat, this is the abstract form of the worker-ownership rule.

### `I2` Queue/history disjointness

A logical user message is never simultaneously:

- present in active history `H` as a committed user message, and
- present in the queue `Q`

A promoted message must leave the queue by the same stable state in which it is
visible in history.

### `I3` Queue order invariant

For queue-preserving transitions:

- untouched queued items preserve relative order
- `AutoPromoteHead` removes only the head
- `DeleteQueued` and `ManualPromote` may remove an arbitrary item, but do not
  reorder survivors

Therefore:

- automatic promotion is FIFO
- manual queue operations are intentionally allowed to violate global FIFO

### `I4` History monotonicity invariant

Ignoring `Edit`, active history `H` is append-only.

`Edit` is the only transition allowed to remove active history, and it may only:

- truncate a suffix beginning at a user message; then
- append exactly one replacement user message

No other transition may reorder or delete active history.

### `I5` Pending-call/status consistency

```text
status = requires_action    iff    pendingCalls ≠ {}
status ≠ requires_action    iff    pendingCalls = {}
```

This is one of the central consistency invariants for the loop.

### `I6` Tool-call closure invariant

At stable points:

- if `status ∉ {running, requires_action}`, active history contains no
  unmatched tool calls
- if `status = requires_action`, the only unmatched tool calls are exactly the
  elements of `pendingCalls`
- those unmatched tool calls are dynamic-tool calls only

### `I7` Busy-interrupt admission invariant

If a user submits a message with interrupt semantics while the chat is busy,
that new user message is admitted into `Q`, not inserted directly into `H`.

This preserves the durable ordering semantics of:

- current assistant progress first (possibly partial)
- promoted replacement user message after that

### `I8` Promotion atomicity invariant

After `ManualPromote` or `AutoPromoteHead`, the promoted logical message is:

- present in `H`
- absent from `Q`

in the same stable state.

No duplicate or limbo state is allowed at cut points.

### `I9` Edit reset invariant

Immediately after `Edit` commits:

- `Q = []`
- `pendingCalls = {}`
- no pre-edit suffix remains in active history
- `status = pending`
- `worker = None`

This captures the logical restart semantics of edit.

### `I10` Requires-action exit closure invariant

Any transition leaving `requires_action` without `SubmitToolResults` must first
close all pending dynamic calls with synthetic error tool results.

This applies to:

- explicit interrupt
- stale recovery timeout

## 5. Liveness properties

These properties require operational assumptions.

### Assumptions

- the daemon wake/acquire loops keep running
- database transactions eventually commit or fail, not hang forever
- model/tool execution eventually returns, errors, is interrupted, or becomes
  stale and recoverable
- no adversary performs infinite conflicting interference on the same chat
  forever (e.g. endless edit/promote/delete races)

Under those assumptions:

### `L1` Pending progress

If a chat remains `pending`, it eventually leaves `pending`.

```text
pending  ⇒  eventually (running ∨ waiting ∨ requires_action ∨ error)
```

In the common case, the first step is `pending -> running`.

### `L2` Running progress

If a chat is `running`, then eventually one of the following occurs:

- a step is committed and the run continues
- the run finishes to `waiting`
- the run enters `requires_action`
- the run fails to `error`
- the worker dies and stale recovery returns it to `pending`

No chat should remain `running` forever without forward progress or recovery.

### `L3` Queue head progress / no starvation

If:

- `Q ≠ []`
- `archived = false`
- the system keeps making progress
- there is no perpetual manual interference

then the head queued message is eventually removed from `Q`, and if removed
automatically it is the next user message promoted into history.

This is the fairness property for the queue-drain mechanism.

### `L4` Interrupt response

Once an interrupt against a running chat takes effect, eventually:

- that run stops owning the chat
- no new model step begins for that same run
- the chat reaches `waiting` or `pending` (if queue auto-promotion follows)

A partial interrupted step may still be durably committed once; that is allowed
by the spec.

### `L5` Requires-action eventual resolution

If a chat enters `requires_action`, then eventually one of the following occurs:

- matching tool results are submitted and the chat becomes `pending`
- the user interrupts and the chat becomes `waiting`
- stale recovery fires and the chat becomes `error`

So `requires_action` is not a sink state.

### `L6` Edit restart progress

After a successful `Edit`, eventually the chat is reprocessed starting from the
replacement message, assuming no further interference prevents progress.

## 6. Notes for implementation review

The two highest-value questions to keep in mind while reviewing code against
this model are:

1. Does every concrete transition refine one of the allowed abstract
   transitions above?
2. At each stable cut point (`CP1..CP5`), do the safety invariants hold even in
   the presence of interference from other allowed transitions?

A particularly important design consequence of the chosen semantics is that
`busy_interrupt` does **not** guarantee immediate preemption. It guarantees safe
admission of the new message into the queue, then attempts best-effort
interruption of the current run.

## 7. Mapping invariants to concrete code

This section maps each abstract invariant to the concrete functions, queries,
and transaction boundaries that are intended to establish or preserve it.
Where the implementation appears weaker than the invariant, that is called out
explicitly as an audit hotspot.

### `I1` Ownership invariant

```text
status = running    iff    worker ≠ None
status ≠ running    iff    worker = None
```

Primary concrete preservers:

- `AcquireChats` in `coderd/database/queries/chats.sql`
  - sets `status = running` and assigns `worker_id`
- `processOnce(...)` in `coderd/x/chatd/chatd.go`
  - the only normal acquisition path into `running`
- `CreateChat(...)`
- `insertUserMessageAndSetPending(...)`
- `EditMessage(...)`
- `PromoteQueued(...)`
- `SubmitToolResults(...)`
- `setChatWaiting(...)`
- `processChat(...)` cleanup transaction
- `recoverStaleChats(...)`
  - all of these move the chat into a non-running status while clearing
    `worker_id`

Supporting control path:

- `subscribeChatControl(...)`
  - ensures a worker gives up its run when control state says it no longer owns
    the chat

Audit hotspot / caveat:

- `acquireManualTitleLock(...)` in `coderd/x/chatd/chatd.go` uses `worker_id`
  as an internal lock token for title regeneration while preserving the current
  chat status. That intentionally violates the raw database-level form of `I1`.
  Therefore `I1` must be interpreted as a **loop-owned-state invariant**, not a
  literal invariant over every chat row regardless of feature scope.

### `I2` Queue/history disjointness

A logical user message must never be simultaneously present in active history
and in the queue.

Primary concrete preservers:

- `SendMessage(...)`
  - queue path inserts into `chat_queued_messages`
  - direct-send path inserts into `chat_messages`
  - it does not do both for the same admitted logical message
- `PromoteQueued(...)`
  - transactionally deletes a queued row and inserts the promoted user message
- `tryAutoPromoteQueuedMessage(...)`
  - pops the head queued row and inserts the promoted user message during
    cleanup
- `DeleteQueued(...)`
- `EditMessage(...)`
  - clears all queued messages, ensuring no stale queued item survives a logical
    restart

Supporting queries:

- `InsertChatQueuedMessage`
- `DeleteChatQueuedMessage`
- `PopNextQueuedMessage`
- `DeleteAllChatQueuedMessages`

Audit hotspot:

- `tryAutoPromoteQueuedMessage(...)` pops the queued row first, then attempts to
  insert the promoted user message. If insertion fails, the function logs and
  returns success-like control to the caller instead of surfacing an error. At
  that point the outer cleanup transaction can still commit. If that path is
  reachable under real write failure, the logical message could end up in
  neither `Q` nor `H`, which would violate the intended disjointness / no-loss
  story.

### `I3` Queue order invariant

Untouched queued items preserve relative order. Automatic promotion is FIFO.
Manual delete/promote may remove arbitrary items without reordering survivors.

Primary concrete preservers:

- `GetChatQueuedMessages` in `coderd/database/queries/chats.sql`
  - orders queue snapshot by `id ASC`
- `InsertChatQueuedMessage`
  - appends by natural increasing row ID
- `PopNextQueuedMessage`
  - removes the minimum-ID queued row, i.e. FIFO head
- `SendMessage(...)`
  - appends new queued messages
- `DeleteQueued(...)`
  - removes one queued row without touching the others
- `PromoteQueued(...)`
  - removes one chosen queued row without reordering survivors
- `tryAutoPromoteQueuedMessage(...)`
  - enforces FIFO for automatic promotion

Audit hotspot:

- same as `I2`: if `tryAutoPromoteQueuedMessage(...)` pops and then fails to
  insert, FIFO order is not merely delayed; the head element can disappear.

### `I4` History monotonicity invariant

Ignoring `Edit`, active history is append-only. `Edit` is the only operation
allowed to truncate active history.

Primary concrete preservers:

- `CreateChat(...)`
  - appends initial system + user messages
- `SendMessage(...)`
  - direct-send path appends a user message
- `persistStep(...)` inside `runChat(...)`
  - appends assistant and tool messages for a completed step
- `persistInterruptedStep(...)` in `coderd/x/chatd/chatloop/chatloop.go`
  - appends partial interrupted assistant/tool state
- `PromoteQueued(...)`
  - appends promoted user message
- `tryAutoPromoteQueuedMessage(...)`
  - appends auto-promoted user message
- `SubmitToolResults(...)`
  - appends tool-result messages
- `insertSyntheticToolResultsTx(...)`
  - appends synthetic error tool results when closing dangling dynamic calls

The single concrete truncation path:

- `EditMessage(...)`
  - `SoftDeleteChatMessageByID`
  - `SoftDeleteChatMessagesAfterID`
  - inserts exactly one replacement user message

Interpretation note:

- This invariant is about **active history**, i.e. rows after soft-deletes are
  projected away. Raw row count in `chat_messages` is not monotone under that
  projection because edit rewrites the logical suffix.

### `I5` Pending-call/status consistency

```text
status = requires_action    iff    pendingCalls ≠ {}
status ≠ requires_action    iff    pendingCalls = {}
```

Concrete representation:

- `pendingCalls` is not stored as a dedicated column.
- It is derived from the last assistant message's dynamic tool calls minus any
  later matching tool-result messages.

Primary concrete preservers:

- `chatloop.Run(...)` in `coderd/x/chatd/chatloop/chatloop.go`
  - detects dynamic tool calls and exits with `ErrDynamicToolCall`
- `runChat(...)`
  - captures `PendingDynamicToolCalls`
- `processChat(...)`
  - sets `status = requires_action` iff pending dynamic calls remain after the
    run
- `SubmitToolResults(...)`
  - validates submitted results against exactly the currently pending dynamic
    tool calls, inserts matching tool-result messages, then sets `status = pending`
- `insertSyntheticToolResultsTx(...)`
  - used by interrupt/recovery paths to discharge pending calls when leaving
    `requires_action`
- `InterruptChat(...)`
- `recoverStaleChats(...)`

Audit hotspot:

- because `pendingCalls` is derived rather than stored, this invariant is not
  local to one function. It depends on the combined behavior of:
  - step persistence,
  - `requires_action` status transitions,
  - tool-result insertion,
  - synthetic closure paths.

### `I6` Tool-call closure invariant

At stable points, unmatched tool calls are allowed only while in
`requires_action`, and only for dynamic tools.

Primary concrete preservers:

- `processStepStream(...)` in `coderd/x/chatd/chatloop/chatloop.go`
  - accumulates tool calls and tool results in one step result
- `executeTools(...)`
  - executes built-in/local tools before the step is persisted
- `persistStep(...)` in `runChat(...)`
  - persists assistant blocks and non-provider-executed tool-result blocks so a
    normal completed step closes its built-in tool calls
- `persistInterruptedStep(...)`
  - synthesizes interrupted tool-result errors for unanswered tool calls when a
    running step is interrupted mid-flight
- dynamic-tool path in `chatloop.Run(...)`
  - leaves calls unmatched intentionally, but only in order to transition the
    chat into `requires_action`
- `SubmitToolResults(...)`
  - closes those unmatched dynamic calls on normal resume
- `insertSyntheticToolResultsTx(...)`
  - closes those unmatched dynamic calls on interrupt / stale timeout

Audit hotspot:

- this invariant straddles `chatloop.go` and `chatd.go`. A review that only
  looks at durable DB mutations without the interrupted-step path will miss an
  important part of the closure guarantee.

### `I7` Busy-interrupt admission invariant

If a user sends a message with interrupt semantics while the chat is busy, the
new message must be admitted to the queue, not inserted directly into history.

Primary concrete preservers:

- `SendMessage(...)`
  - the branch
    `if shouldQueueUserMessage(lockedChat.Status) || len(existingQueued) > 0`
    forces queue admission for busy chats and for chats with backlog
- `shouldQueueUserMessage(...)`
  - defines busy as `{running, pending, requires_action}`
- `SendMessageBusyBehaviorInterrupt`
  - after queue insertion, it calls `setChatWaiting(...)` as a separate
    best-effort interrupt request

Important semantic note:

- This invariant maps to the **best-effort** design: admit first, then request
  interruption. It does not imply immediate preemption.

### `I8` Promotion atomicity invariant

After promotion, the promoted logical message is present in history and absent
from the queue in the same stable state.

Primary concrete preservers:

- `PromoteQueued(...)`
  - runs delete + insert + set pending inside one transaction, so rollback
    preserves atomicity under ordinary failure
- `tryAutoPromoteQueuedMessage(...)`
  - intended to do the same inside `processChat(...)` cleanup transaction

Supporting queries:

- `DeleteChatQueuedMessage`
- `PopNextQueuedMessage`
- `InsertChatMessages`

Audit hotspot:

- `tryAutoPromoteQueuedMessage(...)` again. Because it swallows an insertion
  failure after popping the queued row, the implementation is weaker than the
  invariant unless we assume insertion cannot fail in that location. If we want
  `I8` to be fully true under ordinary DB write failure, this is the first place
  to tighten.

### `I9` Edit reset invariant

Immediately after edit commits:

- `Q = []`
- `pendingCalls = {}`
- no pre-edit suffix remains in active history
- `status = pending`
- `worker = None`

Primary concrete preservers:

- `EditMessage(...)`
  - `SoftDeleteChatMessageByID`
  - `SoftDeleteChatMessagesAfterID`
  - inserts replacement user message
  - `DeleteAllChatQueuedMessages`
  - `UpdateChatStatus(... pending, worker=nil, heartbeat=nil ...)`

Interpretation note:

- `pendingCalls = {}` is achieved indirectly by suffix truncation. Any dynamic
  tool obligations after the edited message are logically removed because their
  originating assistant/tool suffix is soft-deleted.

### `I10` Requires-action exit closure invariant

Any transition that leaves `requires_action` without `SubmitToolResults` must
first append synthetic error tool results for every pending dynamic call.

Primary concrete preservers:

- `SubmitToolResults(...)`
  - the normal, non-synthetic closure path
- `insertSyntheticToolResultsTx(...)`
  - the synthetic closure path used when exiting `requires_action` abnormally
- `InterruptChat(...)`
  - calls `insertSyntheticToolResultsTx(...)` before `setChatWaiting(...)`
- `recoverStaleChats(...)`
  - calls `insertSyntheticToolResultsTx(...)` before setting `status = error`

Audit hotspot:

- both `InterruptChat(...)` and `recoverStaleChats(...)` treat synthetic-tool
  insertion failure as non-fatal and still proceed to leave `requires_action`.
  That means the implementation is again weaker than the invariant unless we
  assume those insertions succeed whenever the exit path commits. If we want
  `I10` to be strict, these two call sites are the key places to change.

## 8. Suggested review order

If the goal is to verify the loop against these invariants, the highest-value
review order is:

1. `SendMessage(...)`
2. `EditMessage(...)`
3. `PromoteQueued(...)`
4. `processChat(...)` cleanup
5. `tryAutoPromoteQueuedMessage(...)`
6. `chatloop.Run(...)` + `persistInterruptedStep(...)`
7. `SubmitToolResults(...)`
8. `InterruptChat(...)`
9. `recoverStaleChats(...)`

That order starts with queue/history transitions, then moves into the dynamic
tool closure and abnormal-exit paths where the currently visible invariant gaps
live.
