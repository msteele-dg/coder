# Plan: State-Aware `PromoteQueued` in chatd

## Context

`PromoteQueued` unconditionally deletes a queued message, inserts it as a user message, and sets the chat to `pending` — regardless of the chat's current status. This is correct for `waiting`/`error` chats but wrong for `running` and `requires_action`.

### Design decisions (from problem description)

1. Manual promotion is always allowed in any state.
2. Model config is NOT preserved from enqueue time (current behavior is acceptable).
3. `requires_action`: close pending dynamic tool calls with synthetic error results (timeout-equivalent closure semantics), then promote normally.
4. `running`: interrupt using the same graceful path as `SendMessage` with interrupt behavior, ensuring partial assistant output persists before the promoted message.

### Key constraint for `running`

The promoted user message must appear in history **after** any partial assistant output from the interrupted run. `persistInterruptedStep` saves partial output during the chatloop unwind, which happens after the interrupt is detected but before the `processChat` deferred cleanup. Therefore we cannot insert the user message in the `PromoteQueued` transaction — it must be deferred to auto-promotion in the worker cleanup, which runs after partial output is persisted.

### Synthetic error message wording

Promotion during `requires_action` uses the same closure semantics as the timeout path (synthetic error tool results for each unresolved dynamic tool call before leaving `requires_action`), but intentionally uses a promotion-specific reason string rather than the timeout reason string:

- Timeout path: `"Dynamic tool execution timed out"`
- Interrupt path: `"Tool execution interrupted by user"`
- Promotion path: `"Tool execution interrupted by queued message promotion"`

### `CreatedBy` trade-off for the `running` case

`tryAutoPromoteQueuedMessage` sets `createdBy = chat.OwnerID` because the `chat_queued_messages` table has no `created_by` column. When promotion is deferred to auto-promote (the `running` case), the promoted message will be attributed to the chat owner, not the user who triggered the promotion. This matches the existing `SendMessage` interrupt behavior and is acceptable for now. A follow-up could add `created_by` to the queued messages table if needed.

A `queue_order` column is added to `chat_queued_messages` to allow stable-ID reordering. The `InsertChatQueuedMessage` query uses `MAX(queue_order) + 1` to assign the next position. This is safe because all current queue mutation flows (`SendMessage`, `PromoteQueued`, `tryAutoPromoteQueuedMessage`, `DeleteQueued`) lock the chat row via `GetChatByIDForUpdate` before mutating queue state, serializing concurrent access. Outside that lock discipline, the `UNIQUE(chat_id, queue_order)` constraint provides the safety net.


### API change: fire-and-forget promotion

The promote endpoint currently returns `200 OK` with a `ChatMessage` body. The frontend treats the returned message as a real durable message and immediately upserts it into both the in-memory store (keyed by `message.id`) and the React Query cache.

This creates two code paths (immediate upsert from HTTP response vs. SSE delivery) for what should be one. The SSE stream already publishes the promoted message via `publishMessage` in all cases, so the HTTP response body is redundant.

This is an intentional API contract change. It is acceptable because:
- the endpoint is under the `/api/experimental/` prefix,
- the primary consumer is the SSE-backed chat UI, and
- the UI is updated in the same change.

If the SSE connection is temporarily down when promotion completes, the client won't see the message until the stream reconnects. The chat page's existing stream reconnect/refetch behavior handles this — on reconnect it re-fetches messages and queue state.

The endpoint is changed to always return `202 Accepted` with a `codersdk.Response` body. The client no longer parses a `ChatMessage` from the response. Instead, it relies entirely on SSE for:
- the promoted message appearing in the timeline,
- queue updates (message removed or reordered), and
- status transitions.

The optimistic update (remove from queue, set status to `"pending"`) still runs before the API call. SSE events correct the optimistic state shortly after. On error, the client rolls back as before.

### Frontend queue suppression for the `running` case

In the `running` case, the backend truthfully publishes the transient reordered queue (e.g. `B, A, C` after promoting `B`). Without client-side handling, this SSE `queue_update` would reintroduce the promoted message into the rendered queue, causing a visible flash before the worker cleanup publishes the final queue (`A, C`).

Because `queue_order` keeps queued-message IDs stable, the frontend can solve this locally with a per-chat suppression set:

- **On promote start**: add the promoted queued-message ID to `suppressedQueuedMessageIDs`.
- **Optimistic local writes** (`setQueuedMessages`): write the filtered queue directly. Do NOT auto-clear suppression — these are not authoritative.
- **Authoritative writes** (SSE `queue_update`, REST/query hydration): use a separate `applyAuthoritativeQueuedMessages` path that filters out suppressed IDs before writing to both the local store AND the React Query cache, then auto-clears suppression for any IDs that are no longer present in the incoming authoritative snapshot (meaning auto-promotion completed).
- **When to clear**: suppression is cleared when (a) an authoritative queue snapshot no longer contains the suppressed ID, (b) the promote request errors and we roll back, or (c) the component unmounts / chat ID changes. Do NOT clear on durable message arrival — `publishMessage` fires before the final `queue_update` in worker cleanup, so clearing too early would allow the queued item to reappear on a refetch.

The distinction between optimistic and authoritative writes is critical. Without it, the optimistic removal of `B` from the local queue would immediately auto-clear `B`'s suppression (since `B` is absent from the optimistic `[A, C]`), and the next SSE `queue_update` with the truthful `[B, A, C]` would reintroduce it.

Note: the chat store uses a custom `state` / `setState` pattern, not Zustand-style `get()` / `set()`. The authoritative path's equality fast-path needs to compare both the filtered queue contents and suppression set changes, otherwise suppressed IDs can get stuck or fail to clear.

This keeps the backend stream semantics truthful while giving the initiating client a clean UX.

## Files Changed

| File | Change |
|---|---|
| `coderd/database/migrations/NNNNNN_chat_queue_order.up.sql` | Add `queue_order` column to `chat_queued_messages` |
| `coderd/database/migrations/NNNNNN_chat_queue_order.down.sql` | Drop `queue_order` column |
| `coderd/database/queries/chats.sql` | Update `InsertChatQueuedMessage`, `GetChatQueuedMessages`, `PopNextQueuedMessage`; add `ReorderChatQueuedMessageToFront` |
| `coderd/x/chatd/chatd.go` | Make `PromoteQueued` state-aware; add `Deferred` field to `PromoteQueuedResult` |
| `coderd/exp_chats.go` | Always return `202 Accepted` (intentional API break; experimental endpoint) |
| `site/src/api/api.ts` | Return `void` instead of `ChatMessage`; expect 202 |
| `site/src/pages/AgentsPage/AgentChatPage.tsx` | Remove durable-message upsert; add suppressed ID on promote |
| `site/src/pages/AgentsPage/components/ChatConversation/chatStore.ts` | Add `suppressedQueuedMessageIDs` set; filter queue state through it |
| `site/src/pages/AgentsPage/components/ChatConversation/useChatStore.ts` | Route SSE `queue_update` and REST/query hydration through authoritative suppression path; propagate filtered queue to React Query cache |

| `coderd/x/chatd/chatd_test.go` | Add tests for `running` and `requires_action` promotion |
| `coderd/exp_chats_test.go` | Add route-level tests for `running` and `requires_action` promotion |

## Phase 1: Red — Write Failing Tests

### Test A: `TestPromoteQueuedWhileRequiresAction`

File: `coderd/x/chatd/chatd_test.go`

Setup:
1. `seedChatDependencies` with a provider that returns a dynamic tool call.
2. `CreateChat` with an initial user message.
3. Wait for the chat to reach `requires_action` status.
4. Send a message with `BusyBehaviorQueue` to create a queued message.

Act:
5. Call `PromoteQueued` with the queued message ID.

Assert:
- No error returned.
- `result.Deferred` is `false`.
- `result.PromotedMessage` has role `user` and the correct content.
- Queue is empty (`GetChatQueuedMessages` returns nothing).
- Chat messages include synthetic error tool results for the pending dynamic tool calls — load tool-role messages from DB, parse their content parts, assert the tool-result parts have `IsError == true` in the parsed content.
- Chat transitions to `pending` and eventually gets processed.

### Test B: `TestPromoteQueuedWhileRequiresActionMixedTools`

File: `coderd/x/chatd/chatd_test.go`

Setup:
1. `seedChatDependencies` with a provider that returns both built-in and dynamic tool calls.
2. `CreateChat`, wait for `requires_action` (built-in tools auto-executed, dynamic tool pending).
3. Queue a message.

Act:
4. Call `PromoteQueued`.

Assert:
- Synthetic error tool results are inserted only for the unresolved dynamic tool calls, not the already-executed built-in ones.
- Promoted message appears after the synthetic results.
- Chat resumes processing.

### Test C: `TestPromoteQueuedWhileRunning`

File: `coderd/x/chatd/chatd_test.go`

Setup:
1. `seedChatDependencies` with a provider whose HTTP handler is held open (channel-gated) so the run stays active.
2. `CreateChat` with an initial message.
3. Wait deterministically for `status == running` in the DB.
4. Send a message with `BusyBehaviorQueue` to create a queued message.

Act:
5. Call `PromoteQueued` with the queued message ID.
6. Release the streaming provider (unblock the channel gate).

Assert:
- No error returned.
- `result.Deferred` is `true`.
- The running worker is interrupted (chat transitions `running` → `waiting` → `pending` via auto-promote).
- After interrupt completes: verify DB message ordering by ID — partial assistant output (if any) has a lower ID than the promoted user message.
- Chat eventually processes the promoted message and reaches `waiting`.

### Test D: `TestPromoteQueuedWhileRunningRespectsMessageOrder`

File: `coderd/x/chatd/chatd_test.go`

Setup:
1. `seedChatDependencies` with a held-open provider.
2. `CreateChat`, wait for `running`.
3. Send messages A, B, C with `BusyBehaviorQueue` (3 queued messages). Record their IDs.

Act:
4. Call `PromoteQueued` for message B (the middle one).
5. Release the streaming provider.

Assert:
- After interrupt + auto-promote, message B is the one promoted into history (content matches B).
- Messages A and C remain in the queue.
- The IDs of messages A and C are unchanged (stable queue IDs — only `queue_order` changed).
- Queue ordering is: A, C (original relative order preserved).

### Test E: Route-level `TestPromoteChatQueuedMessage/WhileRunning`

File: `coderd/exp_chats_test.go`

- POST promote on a `running` chat returns `202 Accepted`.
- Response body is a `codersdk.Response` with an appropriate message (not a `ChatMessage`).
- After the interrupt completes, the promoted message appears in chat history via the normal SSE flow.

### Test F: Route-level `TestPromoteChatQueuedMessage/WhileRequiresAction`

File: `coderd/exp_chats_test.go`

- POST promote on a `requires_action` chat returns `202 Accepted`.
- Synthetic error tool results are present in the chat history.
- The promoted message follows the synthetic results (delivered via SSE).

### Test G: Route-level `TestPromoteChatQueuedMessage/Success` (update existing)

File: `coderd/exp_chats_test.go`

- The existing `Success` subtest currently asserts `200 OK` with a `ChatMessage` body. Update it to assert `202 Accepted` with a `codersdk.Response` body instead.

### Test H: Frontend — promote handler no longer upserts durable message

File: vitest (pure store/mutation logic, no rendered component).

- Mock the promote API to return 202.
- Call `handlePromoteQueuedMessage`.
- Assert: no call to `upsertDurableMessage` or `upsertCacheMessages`.
- Assert: optimistic queue removal still happens.
- Assert: on API error, queue and status are rolled back, suppression is cleared.

### Test I: Frontend — suppression filters intermediate SSE queue_update

File: vitest (pure store logic).

- Simulate: queue has messages A, B, C. User promotes B.
- Assert: `suppressedQueuedMessageIDs` contains B's ID.
- Deliver an authoritative queue update with the transient reordered queue `[B, A, C]` via `applyAuthoritativeQueuedMessages`.
- Assert: the store queue shows `[A, C]` (B is suppressed).
- Deliver the final authoritative queue update with `[A, C]` (auto-promotion completed).
- Assert: the store queue shows `[A, C]`.
- Assert: B is removed from `suppressedQueuedMessageIDs` (auto-cleared because the authoritative queue no longer contains it).

### Test J: Frontend — suppression filters REST/query hydration

File: vitest (pure store/cache logic).

- Simulate: promote B, then a REST refetch/hydration returns queue `[B, A, C]`.
- Assert: the hydrated queue in the store/cache shows `[A, C]` (B is suppressed).


## Phase 2: Green — Implement Changes

### Step 1: Add migration for `queue_order` column

New migration file (next available number):

**Up migration:**

```sql
-- Add queue_order column for stable-ID queue reordering.
-- queue_order determines FIFO position; id remains the stable identity.
ALTER TABLE chat_queued_messages
    ADD COLUMN queue_order BIGINT NOT NULL DEFAULT 0;

-- Initialize queue_order to match existing id ordering.
UPDATE chat_queued_messages SET queue_order = id;

-- Ensure ordering is unique per chat. DEFERRABLE so bulk reorder
-- UPDATEs within a transaction don't hit intermediate violations.
ALTER TABLE chat_queued_messages
    ADD CONSTRAINT uq_chat_queued_messages_order
    UNIQUE (chat_id, queue_order)
    DEFERRABLE INITIALLY DEFERRED;
```

**Down migration:**

```sql
ALTER TABLE chat_queued_messages
    DROP CONSTRAINT IF EXISTS uq_chat_queued_messages_order;
ALTER TABLE chat_queued_messages
    DROP COLUMN IF EXISTS queue_order;
```

### Step 2: Update existing SQL queries in `coderd/database/queries/chats.sql`

**`InsertChatQueuedMessage`** — assign next `queue_order` for the chat:

```sql
-- name: InsertChatQueuedMessage :one
INSERT INTO chat_queued_messages (chat_id, content, queue_order)
VALUES (
    @chat_id,
    @content,
    COALESCE(
        (SELECT MAX(queue_order) + 1
         FROM chat_queued_messages
         WHERE chat_id = @chat_id),
        1
    )
)
RETURNING *;
```

**`GetChatQueuedMessages`** — order by `queue_order`:

```sql
-- name: GetChatQueuedMessages :many
SELECT * FROM chat_queued_messages
WHERE chat_id = @chat_id
ORDER BY queue_order ASC, id ASC;
```

**`PopNextQueuedMessage`** — order by `queue_order`:

```sql
-- name: PopNextQueuedMessage :one
DELETE FROM chat_queued_messages
WHERE id = (
    SELECT cqm.id FROM chat_queued_messages cqm
    WHERE cqm.chat_id = @chat_id
    ORDER BY cqm.queue_order ASC, cqm.id ASC
    LIMIT 1
)
RETURNING *;
```

### Step 3: Add `ReorderChatQueuedMessageToFront` query

Renumber `queue_order` values so the target becomes position 0 and others get sequential values preserving relative order. IDs and `created_at` are not modified.

```sql
-- name: ReorderChatQueuedMessageToFront :exec
-- Moves a queued message to the front of the queue by renumbering
-- queue_order values. The target gets order 0, all others get
-- sequential values (1, 2, ...) preserving their relative order.
-- Requires the DEFERRABLE unique constraint on (chat_id, queue_order).
WITH renumbered AS (
    SELECT id,
           ROW_NUMBER() OVER (
               ORDER BY (id = @target_id) DESC, queue_order ASC, id ASC
           ) - 1 AS new_order
    FROM chat_queued_messages
    WHERE chat_id = @chat_id
)
UPDATE chat_queued_messages cqm
SET queue_order = r.new_order
FROM renumbered r
WHERE cqm.id = r.id AND cqm.chat_id = @chat_id;
```

Note: this query is `:exec`, not `:many`. The returned row order from `UPDATE ... RETURNING` is not guaranteed. Instead, `GetChatQueuedMessages` is called afterward to get the queue in canonical order for publishing.

### Step 4: Run `make -j16 gen`

Regenerate the DB layer after query and migration changes.

### Step 5: Add `Deferred` field to `PromoteQueuedResult`

In `coderd/x/chatd/chatd.go`:

```go
type PromoteQueuedResult struct {
	PromotedMessage database.ChatMessage
	// Deferred is true when the chat was running and promotion is
	// handled via the interrupt+auto-promote path. PromotedMessage
	// is zero-value in this case.
	Deferred bool
}
```

### Step 6: Make `PromoteQueued` state-aware

In `coderd/x/chatd/chatd.go`, modify the `PromoteQueued` function. Inside the transaction, after finding the target queued message (`found = true`), branch on `lockedChat.Status` before the existing delete+insert logic.

Add `needsInterrupt bool` outside the TX closure.

Inside the TX, replace the unconditional delete+insert block with a switch:

```go
switch lockedChat.Status {
case database.ChatStatusRunning:
    // Graceful interrupt path: reorder the queue so the target
    // message is FIFO-first, then set chat to waiting — both in
    // this single TX to avoid a race between reorder and interrupt.
    // The worker's deferred cleanup will auto-promote it after
    // persisting any partial assistant output.
    if err := tx.ReorderChatQueuedMessageToFront(ctx,
        database.ReorderChatQueuedMessageToFrontParams{
            ChatID:   opts.ChatID,
            TargetID: opts.QueuedMessageID,
        }); err != nil {
        return xerrors.Errorf("reorder queued message to front: %w", err)
    }

    // Set chat to waiting in the same TX. This is the same status
    // the SendMessage interrupt path and InterruptChat use.
    updatedChat, err = tx.UpdateChatStatus(ctx, database.UpdateChatStatusParams{
        ID:          opts.ChatID,
        Status:      database.ChatStatusWaiting,
        WorkerID:    uuid.NullUUID{},
        StartedAt:   sql.NullTime{},
        HeartbeatAt: sql.NullTime{},
        LastError:   sql.NullString{},
    })
    if err != nil {
        return xerrors.Errorf("set chat waiting for interrupt: %w", err)
    }

    // Fetch queue in canonical order for the publish payload.
    remainingQueue, err = tx.GetChatQueuedMessages(ctx, opts.ChatID)
    if err != nil {
        return xerrors.Errorf("get remaining queue: %w", err)
    }

    needsInterrupt = true
    return nil

case database.ChatStatusRequiresAction:
    // Close pending dynamic tool calls with synthetic error
    // results before promoting. Uses the same closure semantics
    // as the timeout path but with a promotion-specific reason.
    if err := insertSyntheticToolResultsTx(ctx, tx, lockedChat,
        "Tool execution interrupted by queued message promotion"); err != nil {
        return xerrors.Errorf("insert synthetic tool results: %w", err)
    }
    // Fall through to normal delete + insert + set pending.
    fallthrough

default:
    // waiting, error, pending: normal promotion path.
    // (existing code: delete queued message, insert user message, set pending)

}
```

### Step 7: Update post-TX logic for the interrupt case

After the TX, branch on `needsInterrupt`:

```go
if needsInterrupt {
    result.Deferred = true
    // Publish the truthful reordered queue state. The frontend
    // suppression set prevents the promoted message from flashing
    // back into the rendered queue.
    p.publishEvent(opts.ChatID, codersdk.ChatStreamEvent{
        Type:           codersdk.ChatStreamEventTypeQueueUpdate,
        QueuedMessages: db2sdk.ChatQueuedMessages(remainingQueue),
    })
    p.publishChatStreamNotify(opts.ChatID, coderdpubsub.ChatStreamNotifyMessage{
        QueueUpdate: true,
    })
    // Publish status + control notification so the worker detects
    // the waiting transition and cancels with ErrInterrupted.
    p.publishStatus(opts.ChatID, updatedChat.Status, updatedChat.WorkerID)
    p.publishChatPubsubEvent(updatedChat, coderdpubsub.ChatEventKindStatusChange, nil)
    return result, nil
}
// ... existing post-TX publish logic for non-deferred case ...
```

### Step 8: Update HTTP handler

In `coderd/exp_chats.go`, `promoteChatQueuedMessage` handler. Replace the current `200 OK` + `ChatMessage` response with a uniform `202 Accepted`:

```go
httpapi.Write(ctx, rw, http.StatusAccepted, codersdk.Response{
    Message: "Queued message promotion accepted.",
})
```

The promoted message (for non-deferred cases) is already published via SSE in the post-TX logic of `PromoteQueued`.

### Step 9: Update API client

In `site/src/api/api.ts`, change `promoteChatQueuedMessage` to return `void`:

```ts
promoteChatQueuedMessage = async (
  chatId: string,
  queuedMessageId: number,
): Promise<void> => {
  await this.axios.post(
    `/api/experimental/chats/${chatId}/queue/${queuedMessageId}/promote`,
  );
};
```

### Step 10: Update promote handler in `AgentChatPage.tsx`

Remove the durable-message upsert. Add the promoted ID to the suppression set before the optimistic write.

```ts
const handlePromoteQueuedMessage = async (id: number) => {
  const previousSnapshot = store.getSnapshot();
  const previousQueuedMessages = previousSnapshot.queuedMessages;
  const previousChatStatus = previousSnapshot.chatStatus;

  // Suppress this queued-message ID so authoritative SSE/REST
  // queue updates don't reintroduce it into the rendered queue.
  store.suppressQueuedMessageID(id);

  // Optimistic (non-authoritative): remove from queue immediately.
  // Uses setQueuedMessages which does NOT auto-clear suppression.
  store.setQueuedMessages(
    previousQueuedMessages.filter((message) => message.id !== id),
  );
  store.clearStreamState();
  if (agentId) {
    clearChatErrorReason(agentId);
  }
  store.clearStreamError();
  store.setChatStatus("pending");

  try {
    await promoteQueuedMessage(id);
    // No durable-message upsert — SSE delivers the promoted
    // message, queue updates, and status transitions.
  } catch (error) {
    store.unsuppressQueuedMessageID(id);
    store.setQueuedMessages(previousQueuedMessages);
    store.setChatStatus(previousChatStatus);
    handleUsageLimitError(error);
    throw error;
  }
};
```

### Step 11: Add queue suppression to chatStore

In `site/src/pages/AgentsPage/components/ChatConversation/chatStore.ts`:

1. Add state: `suppressedQueuedMessageIDs: Set<number>` (initialized empty).
2. Add actions:
   - `suppressQueuedMessageID(id: number)` — adds to the set.
   - `unsuppressQueuedMessageID(id: number)` — removes from the set.
3. `setQueuedMessages(messages)` remains the **optimistic / local** write path. It writes `messages` directly to state. It does NOT auto-clear suppression.
4. Add `applyAuthoritativeQueuedMessages(messages)` — the **authoritative** write path used by SSE `queue_update` and REST/query hydration. This function:
   - Filters out any IDs in `suppressedQueuedMessageIDs` before writing.
   - Auto-clears suppression for IDs absent from the incoming authoritative `messages` (auto-promotion completed).
   - Writes the filtered queue to the local store. (The React Query cache write is handled by `useChatStore.ts` — see Step 12.)


The authoritative path logic (illustrative — adapt to the store's `state`/`setState` pattern):
```ts
applyAuthoritativeQueuedMessages: (messages) => {
  const { suppressedQueuedMessageIDs } = state;
  const filtered = messages.filter(
    (m) => !suppressedQueuedMessageIDs.has(m.id),
  );
  // Clear suppression for IDs no longer in the authoritative queue.
  const incomingIDs = new Set(messages.map((m) => m.id));
  const nextSuppressed = new Set(
    [...suppressedQueuedMessageIDs].filter((id) => incomingIDs.has(id)),
  );
  setState({
    queuedMessages: filtered,
    suppressedQueuedMessageIDs: nextSuppressed,
  });
},
```

The equality fast-path in the store must compare both the filtered queue contents AND the suppression set changes. If suppression state changes but the filtered IDs stay the same, the update must still apply so the suppression set gets cleared.

### Step 12: Route SSE and REST/query queue updates through the authoritative path

In `site/src/pages/AgentsPage/components/ChatConversation/useChatStore.ts`:

1. Where SSE `queue_update` events call `store.setQueuedMessages(...)`, change to `store.applyAuthoritativeQueuedMessages(...)`.
2. Where REST/query hydration writes queue state, also use `applyAuthoritativeQueuedMessages(...)` or apply the same suppression filtering before writing to the React Query cache.

The React Query cache write belongs in `useChatStore.ts`, not in `chatStore.ts` (the store itself has no access to the query client). Both the local store and the React Query cache must receive the suppression-filtered queue. `applyAuthoritativeQueuedMessages` handles the store side; `useChatStore.ts` is responsible for propagating the filtered result to the cache.


### Step 13: Clear suppression on unmount

In `AgentChatPage.tsx`, clear the suppression set when the chat component unmounts or the chat ID changes. This prevents stale suppression from leaking across chats.

### Step 14: Update React Query mutation definition

In `site/src/api/queries/chats.ts`, update the `promoteChatQueuedMessage` mutation to match the new `void` return type. The existing definition delegates to `API.experimental.promoteChatQueuedMessage` and has no `onSuccess` handler, so the only change is the inferred return type.

### Step 15: Run `make -j16 gen`, `make -j16 fmt`, `make -j16 lint`

## Phase 3: Refactor

After all tests pass:

1. **DRY up interrupt pattern**: The post-TX notification sequence (publish queue update + publish status + publish pubsub event) is similar between `PromoteQueued` (running case) and `SendMessage` (interrupt case). Consider extracting a shared helper if the duplication is significant.
2. **Review `insertUserMessageAndSetPending`**: Confirm the `requires_action` synthetic-result insertion is correctly scoped to `PromoteQueued` and doesn't belong in the shared utility.
3. **Verify all tests still pass** after refactoring.

## Verification

1. `go test ./coderd/x/chatd/... -run TestPromoteQueued -count=1` — all new backend tests pass.
2. `go test ./coderd/x/chatd/... -count=1` — no regressions in existing chatd tests.
3. `go test ./coderd/... -run TestPromoteChatQueuedMessage -count=1` — HTTP handler tests pass.
4. `make -j16 fmt lint` — clean.

## Todo

- [ ] Add migration for `queue_order` column on `chat_queued_messages`
- [ ] Update `InsertChatQueuedMessage` query to assign next `queue_order`
- [ ] Update `GetChatQueuedMessages` query to order by `queue_order ASC, id ASC`
- [ ] Update `PopNextQueuedMessage` query to order by `queue_order ASC, id ASC`
- [ ] Add `ReorderChatQueuedMessageToFront` query (`:exec`, not `:many`)
- [ ] Run `make -j16 gen` to regenerate DB code
- [ ] Add `Deferred` field to `PromoteQueuedResult` struct
- [ ] Write `TestPromoteQueuedWhileRequiresAction` (red)
- [ ] Write `TestPromoteQueuedWhileRequiresActionMixedTools` (red)
- [ ] Write `TestPromoteQueuedWhileRunning` (red)
- [ ] Write `TestPromoteQueuedWhileRunningRespectsMessageOrder` (red)
- [ ] Write route-level `TestPromoteChatQueuedMessage/WhileRunning` (red)
- [ ] Write route-level `TestPromoteChatQueuedMessage/WhileRequiresAction` (red)
- [ ] Implement `requires_action` branch in `PromoteQueued` (call `insertSyntheticToolResultsTx` before normal promotion)
- [ ] Implement `running` branch in `PromoteQueued` (reorder queue + set `waiting` in same TX, publish after commit)
- [ ] Update HTTP handler in `coderd/exp_chats.go` to always return `202 Accepted`
- [ ] Update `promoteChatQueuedMessage` in `site/src/api/api.ts` to return `void`
- [ ] Update `handlePromoteQueuedMessage` in `site/src/pages/AgentsPage/AgentChatPage.tsx` to remove durable-message upsert and add suppression
- [ ] Add `suppressedQueuedMessageIDs` set and `suppressQueuedMessageID`/`unsuppressQueuedMessageID` actions to `chatStore.ts`
- [ ] Keep `setQueuedMessages` as optimistic-only (no suppression auto-clear)
- [ ] Add `applyAuthoritativeQueuedMessages` to `chatStore.ts` with suppression filtering and auto-clear
- [ ] Route SSE `queue_update` through `applyAuthoritativeQueuedMessages` in `useChatStore.ts`
- [ ] Route REST/query hydration through `applyAuthoritativeQueuedMessages` in `useChatStore.ts`
- [ ] Ensure both local store AND React Query cache receive suppression-filtered queue
- [ ] Clear suppression set on unmount / chat ID change in `AgentChatPage.tsx`
- [ ] Update `promoteChatQueuedMessage` mutation in `site/src/api/queries/chats.ts` for new return type
- [ ] Update existing `TestPromoteChatQueuedMessage/Success` to expect `202`
- [ ] Make `TestPromoteQueuedWhileRequiresAction` pass (green)
- [ ] Make `TestPromoteQueuedWhileRequiresActionMixedTools` pass (green)
- [ ] Make `TestPromoteQueuedWhileRunning` pass (green)
- [ ] Make `TestPromoteQueuedWhileRunningRespectsMessageOrder` pass (green)
- [ ] Make route-level `TestPromoteChatQueuedMessage/WhileRunning` pass (green)
- [ ] Make route-level `TestPromoteChatQueuedMessage/WhileRequiresAction` pass (green)
- [ ] Write frontend test: promote handler no longer upserts durable message
- [ ] Write frontend test: suppression filters intermediate SSE queue_update
- [ ] Write frontend test: suppression filters REST/query hydration
- [ ] Refactor: DRY up interrupt notification pattern if warranted
- [ ] Run `make -j16 gen` after all changes
- [ ] Run `make -j16 fmt`
- [ ] Run `make -j16 lint`
- [ ] Run full chatd test suite: `go test ./coderd/x/chatd/... -count=1`
- [ ] Run HTTP handler tests: `go test ./coderd/... -run TestPromoteChatQueuedMessage -count=1`
