# `PromoteQueued` problem description

## Summary

`PromoteQueued(...)` appears to be **correct in the idle / waiting case**, but it
is likely **semantically incorrect or at least under-specified** when used while
the chat is in other states, especially:

- `running`
- `requires_action`

The implementation currently promotes a queued message by:

1. locking the chat row,
2. deleting the selected queued message,
3. inserting a user message into chat history,
4. setting the chat to `pending`,
5. publishing queue/message/status updates, and
6. waking the daemon.

This is done **without checking the current chat status** or enforcing any
state-specific rules.

## Current implementation behavior

Relevant function:

- `coderd/x/chatd/chatd.go` → `PromoteQueued(...)`

Behavior under lock/transaction:

- `GetChatByIDForUpdate`
- `GetChatQueuedMessages`
- `DeleteChatQueuedMessage`
- `insertUserMessageAndSetPending(...)`
- `GetChatQueuedMessages` (remaining queue)

Post-commit behavior:

- publish `queue_update`
- publish durable promoted user `message`
- publish `status`
- `signalWake()`

## Case analysis

### 1. Waiting / idle chat

This case looks correct.

Expected behavior:

- queued item disappears from queue,
- corresponding user message appears in history,
- chat becomes `pending`,
- daemon picks it up and continues processing.

This appears to match the implementation and existing tests.

### 2. Running chat

This case is suspicious.

What happens today:

- the promoted queued message is inserted,
- the chat is set to `pending`,
- `worker_id` is cleared,
- the old run may still be executing concurrently.

Why this is a problem:

- the old run is not interrupted through the normal interrupt path;
- later step persistence from the old run is rejected once the chat is no
  longer owned/running;
- the old run may continue doing work for some period of time;
- the system does **not** clearly preserve the same graceful semantics as the
  best-effort interrupt path.

In particular, the normal best-effort interrupt design is:

- queue the new message,
- transition to `waiting`,
- allow interrupted partial assistant output to persist.

Manual promotion while `running` does not follow that pattern. Instead, it moves
straight to `pending`, which can cause the old run's future persistence to be
rejected. That makes the behavior look more like abrupt preemption than graceful
interruption.

### 3. `requires_action` chat

This case looks more seriously incorrect.

Meaning of `requires_action`:

- the last assistant turn contains unmatched dynamic tool calls,
- the system is waiting for tool results before safely continuing.

What `PromoteQueued(...)` does today:

- it does **not** check for `requires_action`,
- it does **not** submit tool results,
- it does **not** synthesize tool-result closure,
- it simply inserts a new user message and sets the chat to `pending`.

Why this is a problem:

- the chat can leave `requires_action` without closing pending dynamic tool
  calls;
- the durable history may still contain unmatched dynamic tool calls;
- the chat status becomes `pending`, which is inconsistent with the unresolved
  dynamic-tool state.

This likely violates the intended invariants for:

- pending-call/status consistency,
- tool-call closure, and
- requires-action exit closure.

## Additional concern: model config semantics

`PromoteQueuedOptions` contains an optional `ModelConfigID`, but the HTTP route
for queued-message promotion does not supply one.

As a result, `PromoteQueued(...)` uses:

- `lockedChat.LastModelConfigID`

That means a queued message does **not** preserve the model selection from the
time it was originally queued. If the chat's effective model changed before
promotion, the queued content may run under a different model than the one the
user expected.

This may or may not be acceptable product behavior, but it is another sign that
queued messages are not currently treated as fully self-contained durable user
intent.

## Test coverage gap

Existing tests appear to cover:

- waiting / idle promotion,
- queued promotion after usage-limit changes,
- route-level success and invalid-ID handling.

I did **not** find coverage for:

- promotion while `running`,
- promotion while `requires_action`.

That means the likely problematic states are exactly the ones with the least
explicit verification.

## Provisional conclusion

`PromoteQueued(...)` appears to be:

- **correct in the waiting/idle case**,
- **under-specified and likely semantically wrong in the running case**,
- **likely incorrect in the `requires_action` case**.

The main issue is that promotion currently behaves as a generic
"delete queued row + insert user message + set pending" operation, but the loop
semantics depend heavily on the current execution state. Without state-aware
rules, promotion can violate assumptions that are otherwise maintained by the
normal interrupt, tool-result, and cleanup flows.

## Resolved design decisions

The desired semantics are:

1. **Manual promotion should always be allowed.**
2. **Queued messages should not preserve the model config from enqueue time.**
   Promotion should continue using the chat's current effective model config.
3. **Promotion should be allowed during `requires_action`.** In that case, the
   effect on pending dynamic tool calls should be the same as if they had timed
   out: append synthetic error tool results, clear the pending-call state, then
   continue with the promoted message.
4. **Promotion should be allowed while `running`.** In that case, promotion
   should behave like interrupting the current run and then processing the
   promoted message, preserving the same graceful interruption semantics as the
   best-effort interrupt path.

## Updated acceptance criteria for a correct design

Given the decisions above, a correct queued-message promotion design must be
**state-aware**, not state-restricted.

### Waiting / idle

Promotion should continue to behave as it does today:

- remove the queued row,
- insert the user message into history,
- set the chat to `pending`, and
- wake the daemon.

### Running

Promotion should:

- request interruption using the same semantics as the best-effort interrupt
  path,
- allow interrupted partial assistant output to persist if appropriate,
- ensure the promoted message becomes the next user message processed, and
- avoid silently dropping the old run's in-flight state.

### `requires_action`

Promotion should:

- close pending dynamic tool calls exactly as the timeout path would,
- append synthetic error tool results for unresolved dynamic calls,
- clear the `requires_action` state, and then
- promote and process the queued message.

### Model config

Promotion should use the chat's current effective model config rather than a
model config captured when the message was originally queued.

## Updated conclusion

Under the chosen design, the current implementation remains insufficient:

- it is acceptable for the waiting/idle case,
- it does not implement the desired running-state semantics,
- it does not implement the desired `requires_action` timeout-equivalent
  closure semantics, and
- its current model-config behavior is acceptable by design.

The main missing work is therefore **state-aware promotion logic for**:

- `running`, and
- `requires_action`.
