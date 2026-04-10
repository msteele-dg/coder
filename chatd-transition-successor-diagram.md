# chatd transition successor diagram

## Scope

This document makes the abstract transition system in
`chatd-loop-correctness-model.md` explicit.

It answers:

- what stable state classes exist at cut points,
- which transitions are allowed from each state class, and
- therefore which transitions can immediately follow which other transitions.

This is exhaustive with respect to the abstract transitions defined in the
model, using the model's stable cut points (`CP1..CP5`) and safety invariants.

## Important caveats

1. This document uses **refined stable state classes** so queue occupancy is
   explicit. A plain `status` alone is not enough to derive the successor set.
2. `Edit` is treated as available from every existing chat state because
   `Create(initialUser)` guarantees at least one user message exists.
3. `archived` only matters for `AutoPromoteHead`; that transition is allowed
   only when `archived = false`.
4. The abstract `ManualPromote(qid)` definition only requires `qid ∈ Q`, but if
   it is applied while `status = requires_action` it would leave
   `pendingCalls ≠ {}` while setting `status := pending`, which conflicts with
   invariant `I5`. Therefore this diagram **omits** `A1 -> ManualPromote` from
   the invariant-preserving graph and calls it out separately below as a model
   inconsistency that the redesign should resolve.
5. The current concrete cleanup path may combine run completion and queue
   follow-up at `CP4`. In the abstract graph below there is **no literal**
   `FinishWaiting -> AutoPromoteHead` edge, because `FinishWaiting` requires
   `Q = []` while `AutoPromoteHead` requires `Q ≠ []`.

## Transitions

### External transitions

- `Create(initialUser)` creates a new chat with its initial user turn and lands
  in `pending`.
- `DirectSend(m)` appends a user message directly to an idle chat and lands in
  `pending`.
- `Enqueue(m)` appends a user message to the durable queue without changing the
  active history.
- `ManualPromote(qid)` removes one queued message, appends it to history as a
  user turn, and lands in `pending`.
- `DeleteQueued(qid)` removes one queued message without changing the active
  history.
- `Edit(k, replacement)` truncates active history at a user turn, inserts the
  replacement turn, clears queue and pending calls, and lands in `pending`.
- `SubmitToolResults(results)` appends matching tool results, clears pending
  calls, and lands in `pending`.
- `InterruptRunning(optionalPartialStep)` stops a running chat, optionally
  persists one partial suffix, and lands in `waiting`.
- `InterruptRequiresAction(reason)` closes pending calls with synthetic error
  tool results and lands in `waiting`.

### Worker transitions

- `Acquire(w)` claims a `pending` chat for worker `w` and moves it to
  `running`.
- `CommitStep(step)` appends one durable assistant/tool suffix while remaining
  `running`.
- `EnterRequiresAction(calls)` records pending dynamic tool calls and lands in
  `requires_action`.
- `FinishWaiting` completes a run with no backlog and lands in `waiting`.
- `AutoPromoteHead` promotes the queue head from `waiting` to `pending` when
  backlog exists and the chat is not archived.
- `FinishError(err)` ends a running chat in `error`.

### Recovery transitions

- `RecoverStaleRunning` returns a stale `running` chat to `pending`.
- `RecoverStaleRequiresAction(reason)` closes pending calls with synthetic error
  tool results and lands in `error`.

## Refined stable state classes

| Code | Meaning |
|---|---|
| `N` | chat does not exist |
| `W0` | `status=waiting`, `Q=[]` |
| `W1` | `status=waiting`, `Q≠[]` |
| `E0` | `status=error`, `Q=[]` |
| `E1` | `status=error`, `Q≠[]` |
| `P0` | `status=pending`, `Q=[]` |
| `P1` | `status=pending`, `Q≠[]` |
| `R0` | `status=running`, `Q=[]` |
| `R1` | `status=running`, `Q≠[]` |
| `A0` | `status=requires_action`, `Q=[]`, `pendingCalls≠{}` |
| `A1` | `status=requires_action`, `Q≠[]`, `pendingCalls≠{}` |

## Exhaustive refined state diagram

```mermaid
stateDiagram-v2
    direction LR

    [*] --> N

    N --> P0: Create

    W0 --> P0: DirectSend
    W0 --> P0: Edit

    W1 --> W1: Enqueue
    W1 --> W0: DeleteQueued / removed last queued
    W1 --> W1: DeleteQueued / queue still non-empty
    W1 --> P0: ManualPromote / promoted last queued
    W1 --> P1: ManualPromote / queue still non-empty
    W1 --> P0: Edit
    W1 --> P0: AutoPromoteHead / archived=false and tail becomes empty
    W1 --> P1: AutoPromoteHead / archived=false and tail remains

    E0 --> P0: DirectSend
    E0 --> P0: Edit

    E1 --> E1: Enqueue
    E1 --> E0: DeleteQueued / removed last queued
    E1 --> E1: DeleteQueued / queue still non-empty
    E1 --> P0: ManualPromote / promoted last queued
    E1 --> P1: ManualPromote / queue still non-empty
    E1 --> P0: Edit

    P0 --> R0: Acquire
    P0 --> P1: Enqueue
    P0 --> P0: Edit

    P1 --> R1: Acquire
    P1 --> P1: Enqueue
    P1 --> P0: DeleteQueued / removed last queued
    P1 --> P1: DeleteQueued / queue still non-empty
    P1 --> P0: ManualPromote / promoted last queued
    P1 --> P1: ManualPromote / queue still non-empty
    P1 --> P0: Edit

    R0 --> R0: CommitStep
    R0 --> A0: EnterRequiresAction
    R0 --> W0: FinishWaiting
    R0 --> E0: FinishError
    R0 --> P0: RecoverStaleRunning
    R0 --> W0: InterruptRunning
    R0 --> R1: Enqueue
    R0 --> P0: Edit

    R1 --> R1: CommitStep
    R1 --> A1: EnterRequiresAction
    R1 --> E1: FinishError
    R1 --> P1: RecoverStaleRunning
    R1 --> W1: InterruptRunning
    R1 --> R1: Enqueue
    R1 --> R0: DeleteQueued / removed last queued
    R1 --> R1: DeleteQueued / queue still non-empty
    R1 --> P0: ManualPromote / promoted last queued
    R1 --> P1: ManualPromote / queue still non-empty
    R1 --> P0: Edit

    A0 --> P0: SubmitToolResults
    A0 --> W0: InterruptRequiresAction
    A0 --> E0: RecoverStaleRequiresAction
    A0 --> A1: Enqueue
    A0 --> P0: Edit

    A1 --> P1: SubmitToolResults
    A1 --> W1: InterruptRequiresAction
    A1 --> E1: RecoverStaleRequiresAction
    A1 --> A1: Enqueue
    A1 --> A0: DeleteQueued / removed last queued
    A1 --> A1: DeleteQueued / queue still non-empty
    A1 --> P0: Edit
```

## Model inconsistency to resolve in the redesign

As written, `ManualPromote(qid)` only requires `qid ∈ Q`. That would make it
appear callable from `A1`.

But its listed effects are:

- remove `qid` from `Q`
- append `User(qid.content)` to `H`
- `status := pending`
- `worker := None`

It does **not** clear `pendingCalls`.

So if applied from `A1`, it would produce a state with:

- `status = pending`
- `pendingCalls ≠ {}`

which conflicts with invariant `I5` (`status = requires_action iff pendingCalls ≠ {}`).

The redesign should therefore do one of the following explicitly:

1. reject `PromoteQueued` while `status = requires_action`, or
2. redefine `ManualPromote` so it also clears/closes `pendingCalls`, or
3. give it a different command/effect meaning in the actor model.

## Composite-operation notes

### Busy interrupt

`SendMessage` with interrupt semantics is **not** a single abstract transition.
It is modeled as:

1. `Enqueue(m)`
2. best-effort `InterruptRunning(...)`

So when looking for successors, treat those as two separate steps.

### Run completion with queue follow-up

The concrete implementation may combine release and queue promotion in one
cleanup path at `CP4`.

The abstract graph in this document keeps:

- `FinishWaiting` for `running -> waiting` when `Q=[]`, and
- `AutoPromoteHead` for `waiting -> pending` when `Q≠[]` and `archived=false`.

That means there is no direct `FinishWaiting -> AutoPromoteHead` edge in the
refined invariant-preserving graph.
