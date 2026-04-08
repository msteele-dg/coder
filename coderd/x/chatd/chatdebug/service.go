package chatdebug

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/pubsub"
)

// DefaultStaleThreshold is the fallback stale timeout for debug rows
// when no caller-provided value is supplied.
const DefaultStaleThreshold = 5 * time.Minute

// Service persists chat debug rows and fans out lightweight change events.
type Service struct {
	db           database.Store
	log          slog.Logger
	pubsub       pubsub.Pubsub
	alwaysEnable bool
	// staleAfterNanos stores the stale threshold as nanoseconds in an
	// atomic.Int64 so SetStaleAfter and FinalizeStale can be called
	// from concurrent goroutines without a data race.
	staleAfterNanos atomic.Int64
}

// ServiceOption configures optional Service behavior.
type ServiceOption func(*Service)

// WithStaleThreshold overrides the default stale-row finalization
// threshold. Callers that already have a configurable in-flight chat
// timeout (e.g. chatd's InFlightChatStaleAfter) should pass it here
// so the two sweeps stay in sync.
func WithStaleThreshold(d time.Duration) ServiceOption {
	return func(s *Service) {
		if d > 0 {
			s.staleAfterNanos.Store(d.Nanoseconds())
		}
	}
}

// WithAlwaysEnable forces debug logging on for every chat regardless
// of the runtime admin and user opt-in settings. This is used for the
// deployment-level serpent flag.
func WithAlwaysEnable(always bool) ServiceOption {
	return func(s *Service) {
		s.alwaysEnable = always
	}
}

// CreateRunParams contains friendly inputs for creating a debug run.
type CreateRunParams struct {
	ChatID              uuid.UUID
	RootChatID          uuid.UUID
	ParentChatID        uuid.UUID
	ModelConfigID       uuid.UUID
	TriggerMessageID    int64
	HistoryTipMessageID int64
	Kind                RunKind
	Status              Status
	Provider            string
	Model               string
	Summary             any
}

// UpdateRunParams contains inputs for updating a debug run.
// Zero-valued fields are treated as "keep the existing value" by the
// COALESCE-based SQL query.  Once a field is set it cannot be cleared
// back to NULL — this is intentional for the write-once-finalize
// lifecycle of debug rows.
type UpdateRunParams struct {
	ID         uuid.UUID
	ChatID     uuid.UUID
	Status     Status
	Summary    any
	FinishedAt time.Time
}

// CreateStepParams contains friendly inputs for creating a debug step.
type CreateStepParams struct {
	RunID               uuid.UUID
	ChatID              uuid.UUID
	StepNumber          int32
	Operation           Operation
	Status              Status
	HistoryTipMessageID int64
	NormalizedRequest   any
}

// UpdateStepParams contains optional inputs for updating a debug step.
// Most payload fields are typed as any and serialized through nullJSON
// because their shape varies by provider.  The Attempts field uses a
// concrete slice for compile-time safety where the schema is stable.
// Zero-valued fields are treated as "keep the existing value" by the
// COALESCE-based SQL query — once set, fields cannot be cleared back
// to NULL.  This is intentional for the write-once-finalize lifecycle
// of debug rows.
type UpdateStepParams struct {
	ID                 uuid.UUID
	ChatID             uuid.UUID
	Status             Status
	AssistantMessageID int64
	NormalizedResponse any
	Usage              any
	Attempts           []Attempt
	Error              any
	Metadata           any
	FinishedAt         time.Time
}

// NewService constructs a chat debug persistence service.
func NewService(db database.Store, log slog.Logger, ps pubsub.Pubsub, opts ...ServiceOption) *Service {
	if db == nil {
		panic("chatdebug: nil database.Store")
	}

	s := &Service{
		db:     db,
		log:    log,
		pubsub: ps,
	}
	s.staleAfterNanos.Store(DefaultStaleThreshold.Nanoseconds())
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SetStaleAfter overrides the in-flight stale threshold used when
// finalizing abandoned debug rows. Zero or negative durations keep the
// default threshold.
func (s *Service) SetStaleAfter(staleAfter time.Duration) {
	if s == nil || staleAfter <= 0 {
		return
	}
	s.staleAfterNanos.Store(staleAfter.Nanoseconds())
}

// staleThreshold returns the current stale timeout.
func (s *Service) staleThreshold() time.Duration {
	ns := s.staleAfterNanos.Load()
	d := time.Duration(ns)
	if d <= 0 {
		return DefaultStaleThreshold
	}
	return d
}

// heartbeatInterval returns a safe ticker interval for stream heartbeats.
// It is half the stale threshold, clamped to at least 1 second to prevent
// panics from time.NewTicker(0) with pathologically small thresholds.
func (s *Service) heartbeatInterval() time.Duration {
	interval := s.staleThreshold() / 2
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

func chatdContext(ctx context.Context) context.Context {
	//nolint:gocritic // AsChatd provides narrowly-scoped daemon access for
	// chat debug persistence reads and writes.
	return dbauthz.AsChatd(ctx)
}

// IsEnabled returns whether debug logging is enabled for the given chat.
func (s *Service) IsEnabled(
	ctx context.Context,
	chatID uuid.UUID,
	ownerID uuid.UUID,
) bool {
	if s == nil {
		return false
	}
	if s.alwaysEnable {
		return true
	}
	if s.db == nil {
		return false
	}

	authCtx := chatdContext(ctx)

	allowUsers, err := s.db.GetChatDebugLoggingAllowUsers(authCtx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		s.log.Warn(ctx, "failed to load runtime admin chat debug logging setting",
			slog.Error(err),
		)
		return false
	}
	if !allowUsers {
		return false
	}

	if ownerID == uuid.Nil {
		s.log.Warn(ctx, "missing chat owner for debug logging enablement check",
			slog.F("chat_id", chatID),
		)
		return false
	}

	enabled, err := s.db.GetUserChatDebugLoggingEnabled(authCtx, ownerID)
	if err == nil {
		return enabled
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}

	s.log.Warn(ctx, "failed to load user chat debug logging setting",
		slog.Error(err),
		slog.F("chat_id", chatID),
		slog.F("owner_id", ownerID),
	)
	return false
}

// CreateRun inserts a new debug run and emits a run update event.
func (s *Service) CreateRun(
	ctx context.Context,
	params CreateRunParams,
) (database.ChatDebugRun, error) {
	run, err := s.db.InsertChatDebugRun(chatdContext(ctx),
		database.InsertChatDebugRunParams{
			ChatID:              params.ChatID,
			RootChatID:          nullUUID(params.RootChatID),
			ParentChatID:        nullUUID(params.ParentChatID),
			ModelConfigID:       nullUUID(params.ModelConfigID),
			TriggerMessageID:    nullInt64(params.TriggerMessageID),
			HistoryTipMessageID: nullInt64(params.HistoryTipMessageID),
			Kind:                string(params.Kind),
			Status:              string(params.Status),
			Provider:            nullString(params.Provider),
			Model:               nullString(params.Model),
			Summary:             s.nullJSON(ctx, params.Summary),
			StartedAt:           sql.NullTime{},
			UpdatedAt:           sql.NullTime{},
			FinishedAt:          sql.NullTime{},
		})
	if err != nil {
		return database.ChatDebugRun{}, err
	}

	s.publishEvent(ctx, run.ChatID, EventKindRunUpdate, run.ID, uuid.Nil)
	return run, nil
}

// UpdateRun updates an existing debug run and emits a run update event.
func (s *Service) UpdateRun(
	ctx context.Context,
	params UpdateRunParams,
) (database.ChatDebugRun, error) {
	run, err := s.db.UpdateChatDebugRun(chatdContext(ctx),
		database.UpdateChatDebugRunParams{
			RootChatID:          uuid.NullUUID{},
			ParentChatID:        uuid.NullUUID{},
			ModelConfigID:       uuid.NullUUID{},
			TriggerMessageID:    sql.NullInt64{},
			HistoryTipMessageID: sql.NullInt64{},
			Status:              nullString(string(params.Status)),
			Provider:            sql.NullString{},
			Model:               sql.NullString{},
			Summary:             s.nullJSON(ctx, params.Summary),
			FinishedAt:          nullTime(params.FinishedAt),
			ID:                  params.ID,
			ChatID:              params.ChatID,
		})
	if err != nil {
		return database.ChatDebugRun{}, err
	}

	s.publishEvent(ctx, run.ChatID, EventKindRunUpdate, run.ID, uuid.Nil)
	return run, nil
}

// CreateStep inserts a new debug step and emits a step update event.
func (s *Service) CreateStep(
	ctx context.Context,
	params CreateStepParams,
) (database.ChatDebugStep, error) {
	insert := database.InsertChatDebugStepParams{
		RunID:               params.RunID,
		StepNumber:          params.StepNumber,
		Operation:           string(params.Operation),
		Status:              string(params.Status),
		HistoryTipMessageID: nullInt64(params.HistoryTipMessageID),
		AssistantMessageID:  sql.NullInt64{},
		NormalizedRequest:   s.nullJSON(ctx, params.NormalizedRequest),
		NormalizedResponse:  pqtype.NullRawMessage{},
		Usage:               pqtype.NullRawMessage{},
		Attempts:            pqtype.NullRawMessage{},
		Error:               pqtype.NullRawMessage{},
		Metadata:            pqtype.NullRawMessage{},
		StartedAt:           sql.NullTime{},
		UpdatedAt:           sql.NullTime{},
		FinishedAt:          sql.NullTime{},
		ChatID:              params.ChatID,
	}

	// Cap retry attempts to prevent infinite loops under
	// pathological concurrency. Each iteration performs two DB
	// round-trips (insert + list), so 10 retries is generous.
	const maxCreateStepRetries = 10

	for attempt := 0; attempt < maxCreateStepRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return database.ChatDebugStep{}, err
		}

		step, err := s.db.InsertChatDebugStep(chatdContext(ctx), insert)
		if err == nil {
			// Touch the parent run's updated_at so the stale-
			// finalization sweep does not prematurely interrupt
			// long-running runs that are still producing steps.
			if _, touchErr := s.db.UpdateChatDebugRun(chatdContext(ctx), database.UpdateChatDebugRunParams{
				RootChatID:          uuid.NullUUID{},
				ParentChatID:        uuid.NullUUID{},
				ModelConfigID:       uuid.NullUUID{},
				TriggerMessageID:    sql.NullInt64{},
				HistoryTipMessageID: sql.NullInt64{},
				Status:              sql.NullString{},
				Provider:            sql.NullString{},
				Model:               sql.NullString{},
				Summary:             pqtype.NullRawMessage{},
				FinishedAt:          sql.NullTime{},
				ID:                  params.RunID,
				ChatID:              params.ChatID,
			}); touchErr != nil {
				s.log.Warn(ctx, "failed to touch parent run updated_at",
					slog.F("run_id", params.RunID),
					slog.Error(touchErr),
				)
			}
			s.publishEvent(ctx, step.ChatID, EventKindStepUpdate, step.RunID, step.ID)
			return step, nil
		}
		if !database.IsUniqueViolation(err, database.UniqueIndexChatDebugStepsRunStep) {
			return database.ChatDebugStep{}, err
		}

		steps, listErr := s.db.GetChatDebugStepsByRunID(chatdContext(ctx), params.RunID)
		if listErr != nil {
			return database.ChatDebugStep{}, listErr
		}
		nextStepNumber := insert.StepNumber + 1
		for _, existing := range steps {
			if existing.StepNumber >= nextStepNumber {
				nextStepNumber = existing.StepNumber + 1
			}
		}
		insert.StepNumber = nextStepNumber
	}

	return database.ChatDebugStep{}, xerrors.Errorf(
		"chatdebug: failed to create step after %d retries (run %s)",
		maxCreateStepRetries, params.RunID,
	)
}

// UpdateStep updates an existing debug step and emits a step update event.
func (s *Service) UpdateStep(
	ctx context.Context,
	params UpdateStepParams,
) (database.ChatDebugStep, error) {
	step, err := s.db.UpdateChatDebugStep(chatdContext(ctx),
		database.UpdateChatDebugStepParams{
			Status:              nullString(string(params.Status)),
			HistoryTipMessageID: sql.NullInt64{},
			AssistantMessageID:  nullInt64(params.AssistantMessageID),
			NormalizedRequest:   pqtype.NullRawMessage{},
			NormalizedResponse:  s.nullJSON(ctx, params.NormalizedResponse),
			Usage:               s.nullJSON(ctx, params.Usage),
			Attempts:            s.nullJSON(ctx, params.Attempts),
			Error:               s.nullJSON(ctx, params.Error),
			Metadata:            s.nullJSON(ctx, params.Metadata),
			FinishedAt:          nullTime(params.FinishedAt),
			ID:                  params.ID,
			ChatID:              params.ChatID,
		})
	if err != nil {
		return database.ChatDebugStep{}, err
	}

	s.publishEvent(ctx, step.ChatID, EventKindStepUpdate, step.RunID, step.ID)
	return step, nil
}

// TouchStep bumps the step's and its parent run's updated_at timestamps
// without changing any other fields. This prevents long-running operations
// (e.g. streaming) from being prematurely swept by FinalizeStale, which
// first marks runs stale by chat_debug_runs.updated_at and then cascades
// to steps whose run_id was just finalized.
func (s *Service) TouchStep(
	ctx context.Context,
	stepID uuid.UUID,
	runID uuid.UUID,
	chatID uuid.UUID,
) error {
	authCtx := chatdContext(ctx)
	// All nullable fields are zero → COALESCE preserves existing
	// values. Only updated_at = NOW() is applied by the query.
	_, err := s.db.UpdateChatDebugStep(authCtx,
		database.UpdateChatDebugStepParams{
			ID:                  stepID,
			ChatID:              chatID,
			Status:              sql.NullString{},
			HistoryTipMessageID: sql.NullInt64{},
			AssistantMessageID:  sql.NullInt64{},
			NormalizedRequest:   pqtype.NullRawMessage{},
			NormalizedResponse:  pqtype.NullRawMessage{},
			Usage:               pqtype.NullRawMessage{},
			Attempts:            pqtype.NullRawMessage{},
			Error:               pqtype.NullRawMessage{},
			Metadata:            pqtype.NullRawMessage{},
			FinishedAt:          sql.NullTime{},
		})
	if err != nil {
		return err
	}
	// Also touch the parent run so the stale sweep does not finalize
	// it (and cascade-interrupt this step) while the stream is alive.
	_, err = s.db.UpdateChatDebugRun(authCtx,
		database.UpdateChatDebugRunParams{
			ID:                  runID,
			ChatID:              chatID,
			RootChatID:          uuid.NullUUID{},
			ParentChatID:        uuid.NullUUID{},
			ModelConfigID:       uuid.NullUUID{},
			TriggerMessageID:    sql.NullInt64{},
			HistoryTipMessageID: sql.NullInt64{},
			Status:              sql.NullString{},
			Provider:            sql.NullString{},
			Model:               sql.NullString{},
			Summary:             pqtype.NullRawMessage{},
			FinishedAt:          sql.NullTime{},
		})
	return err
}

// DeleteByChatID deletes all debug data for a chat and emits a delete event.
func (s *Service) DeleteByChatID(
	ctx context.Context,
	chatID uuid.UUID,
) (int64, error) {
	deleted, err := s.db.DeleteChatDebugDataByChatID(chatdContext(ctx), chatID)
	if err != nil {
		return 0, err
	}

	s.publishEvent(ctx, chatID, EventKindDelete, uuid.Nil, uuid.Nil)
	return deleted, nil
}

// DeleteAfterMessageID deletes debug data newer than the given message.
func (s *Service) DeleteAfterMessageID(
	ctx context.Context,
	chatID uuid.UUID,
	messageID int64,
) (int64, error) {
	deleted, err := s.db.DeleteChatDebugDataAfterMessageID(
		chatdContext(ctx),
		database.DeleteChatDebugDataAfterMessageIDParams{
			ChatID:    chatID,
			MessageID: messageID,
		},
	)
	if err != nil {
		return 0, err
	}

	s.publishEvent(ctx, chatID, EventKindDelete, uuid.Nil, uuid.Nil)
	return deleted, nil
}

// FinalizeStale finalizes stale in-flight debug rows and emits a broadcast.
func (s *Service) FinalizeStale(
	ctx context.Context,
) (database.FinalizeStaleChatDebugRowsRow, error) {
	ns := s.staleAfterNanos.Load()
	staleAfter := time.Duration(ns)
	if staleAfter <= 0 {
		staleAfter = DefaultStaleThreshold
	}

	result, err := s.db.FinalizeStaleChatDebugRows(
		chatdContext(ctx),
		time.Now().Add(-staleAfter),
	)
	if err != nil {
		return database.FinalizeStaleChatDebugRowsRow{}, err
	}

	if result.RunsFinalized > 0 || result.StepsFinalized > 0 {
		s.publishEvent(ctx, uuid.Nil, EventKindFinalize, uuid.Nil, uuid.Nil)
	}
	return result, nil
}

func nullUUID(id uuid.UUID) uuid.NullUUID {
	return uuid.NullUUID{UUID: id, Valid: id != uuid.Nil}
}

func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullTime(value time.Time) sql.NullTime {
	return sql.NullTime{Time: value, Valid: !value.IsZero()}
}

// jsonClear is a sentinel value that tells nullJSON to emit a valid
// JSON null (JSONB 'null') instead of SQL NULL.  COALESCE treats SQL
// NULL as "keep existing" but replaces with a non-NULL JSONB value,
// so passing jsonClear explicitly overwrites a previously set field.
type jsonClear struct{}

// nullJSON marshals value to a NullRawMessage. When value is nil or
// marshals to JSON "null", the result is {Valid: false}. Combined with
// the COALESCE-based UPDATE queries, this means a caller cannot clear a
// previously-set JSON column back to NULL — passing nil preserves the
// existing value. This is acceptable for debug logs because fields
// accumulate monotonically (request → response → usage → error) and
// never need to be cleared during normal operation.
func (s *Service) nullJSON(ctx context.Context, value any) pqtype.NullRawMessage {
	if value == nil {
		return pqtype.NullRawMessage{}
	}
	// Sentinel: emit a valid JSONB null so COALESCE replaces
	// any previously stored value.
	if _, ok := value.(jsonClear); ok {
		return pqtype.NullRawMessage{
			RawMessage: json.RawMessage("null"),
			Valid:      true,
		}
	}

	data, err := json.Marshal(value)
	if err != nil {
		s.log.Warn(ctx, "failed to marshal chat debug JSON",
			slog.Error(err),
			slog.F("value_type", fmt.Sprintf("%T", value)),
		)
		return pqtype.NullRawMessage{}
	}
	if bytes.Equal(data, []byte("null")) {
		return pqtype.NullRawMessage{}
	}

	return pqtype.NullRawMessage{RawMessage: data, Valid: true}
}

func (s *Service) publishEvent(
	ctx context.Context,
	chatID uuid.UUID,
	kind EventKind,
	runID uuid.UUID,
	stepID uuid.UUID,
) {
	if s.pubsub == nil {
		s.log.Debug(ctx,
			"chat debug pubsub unavailable; skipping event",
			slog.F("kind", kind),
			slog.F("chat_id", chatID),
		)
		return
	}

	event := DebugEvent{
		Kind:   kind,
		ChatID: chatID,
		RunID:  runID,
		StepID: stepID,
	}
	data, err := json.Marshal(event)
	if err != nil {
		s.log.Warn(ctx, "failed to marshal chat debug event",
			slog.Error(err),
			slog.F("kind", kind),
			slog.F("chat_id", chatID),
		)
		return
	}

	channel := PubsubChannel(chatID)
	if err := s.pubsub.Publish(channel, data); err != nil {
		s.log.Warn(ctx, "failed to publish chat debug event",
			slog.Error(err),
			slog.F("channel", channel),
			slog.F("kind", kind),
			slog.F("chat_id", chatID),
		)
	}
}
