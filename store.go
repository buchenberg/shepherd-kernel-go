package shepherd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrUnknownFrontier is the expected "no retained cutoff for this frontier id"
// condition, distinguished from real read faults so callers can retry it
// deliberately instead of spinning on every error.
var ErrUnknownFrontier = errors.New("unknown frontier")

// unknownFrontierError keeps the store's message shape while matching the
// sentinel through errors.Is.
type unknownFrontierError struct{ frontierID string }

func (e *unknownFrontierError) Error() string {
	return fmt.Sprintf("unknown frontier id: %q", e.frontierID)
}

func (e *unknownFrontierError) Is(target error) bool { return target == ErrUnknownFrontier }

// TraceStoreError is the base error for trace store law violations.
type TraceStoreError struct {
	msg string
}

func (e *TraceStoreError) Error() string { return e.msg }

// AppendIntentConflictError is raised when an append intent is retried with a different batch.
type AppendIntentConflictError struct {
	msg string
}

func (e *AppendIntentConflictError) Error() string { return e.msg }

// UnknownFactError is raised when a retained fact references a missing fact.
type UnknownFactError struct {
	msg string
}

func (e *UnknownFactError) Error() string { return e.msg }

const witnessTraceOwnerID = "kernel:witness"

// SQLiteTraceStore is the canonical TraceStore implementation backed by SQLite.
type SQLiteTraceStore struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
	bus  *EffectBus // optional; nil = no real-time publishing
}

// WithBus attaches an effect bus to the store. When set, every successful
// Append publishes an EffectEvent for each retained record. This enables
// real-time supervision without modifying the store's persistence behavior.
//
// The bus is optional — existing code that doesn't set a bus sees no change.
func (s *SQLiteTraceStore) WithBus(bus *EffectBus) *SQLiteTraceStore {
	s.bus = bus
	return s
}

// NewSQLiteTraceStore creates a new trace store at the given path.
// Use ":memory:" for an in-memory store.
func NewSQLiteTraceStore(path string) (*SQLiteTraceStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer

	// WAL mode allows concurrent readers with a single writer.
	// Busy timeout makes SQLite wait for the lock instead of returning
	// SQLITE_BUSY immediately when the middleware and tools write
	// concurrently.
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	s := &SQLiteTraceStore{path: path, db: db}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *SQLiteTraceStore) Close(ctx context.Context) error {
	return s.db.Close()
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// q returns the transaction if non-nil, otherwise the raw db.
func (s *SQLiteTraceStore) q(tx *sql.Tx) queryer {
	if tx != nil {
		return tx
	}
	return s.db
}

// Append appends a semantic batch or returns the prior receipt for its intent.
func (s *SQLiteTraceStore) Append(ctx context.Context, auth AppendContext, batch AppendBatch) (AppendReceipt, error) {
	opCtx := auth.ToOperationContext(OpAppend)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return AppendReceipt{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return AppendReceipt{}, fmt.Errorf("begin transaction: %w", err)
	}

	receipt, isNew, err := s.appendInTx(tx, opCtx, batch)
	if err != nil {
		tx.Rollback()
		return AppendReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return AppendReceipt{}, fmt.Errorf("commit: %w", err)
	}

	// Publish effect events after successful commit. Only for new appends,
	// not idempotent retries — the store is the durable record, the bus is
	// ephemeral. Idempotent appends return existing receipts without
	// inserting new records, so publishing would be a no-op at best.
	if isNew {
		s.publishAppendEvents(batch, receipt)
	}

	return receipt, nil
}

// publishAppendEvents publishes an EffectEvent for each retained record
// in a successfully committed batch. Called after tx.Commit() so the bus
// never blocks or fails the persistence path.
func (s *SQLiteTraceStore) publishAppendEvents(batch AppendBatch, receipt AppendReceipt) {
	if s.bus == nil {
		return
	}

	now := time.Now()
	factIdx := 0
	for _, group := range batch.Groups {
		for _, draft := range group.FactDrafts {
			if factIdx >= len(receipt.FactIDs) {
				return
			}
			event := EffectEvent{
				RecordID:      receipt.FactIDs[factIdx],
				IntentID:      batch.AppendIntentID,
				TraceOwnerID:  group.TraceOwnerID,
				Mode:          draft.Mode,
				SchemaRef:     draft.SchemaRef,
				KindLabel:     draft.KindLabel,
				Payload:       draft.Payload,
				CausalParents: append(group.CausalParents, draft.CausedByFactIDs...),
				Timestamp:     now,
			}
			s.bus.Publish(event)
			factIdx++
		}
	}
}

// PreviewRecordIDs returns the durable record IDs this batch would allocate if committed.
func (s *SQLiteTraceStore) PreviewRecordIDs(ctx context.Context, auth AppendContext, batch AppendBatch) ([]string, error) {
	opCtx := auth.ToOperationContext(OpAppend)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return nil, err
	}
	if err := validateBatchShape(batch, opCtx); err != nil {
		return nil, err
	}
	return s.previewFactIDs(ctx, batch, opCtx)
}

// ReadFact reads one retained fact.
func (s *SQLiteTraceStore) ReadFact(ctx context.Context, auth ReadContext, factID string) (VisibleRecord, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return nil, err
	}
	return s.readFact(ctx, factID)
}

// ReadOwnerPrefix reads all facts on an owner path up to and including the given ordinal.
func (s *SQLiteTraceStore) ReadOwnerPrefix(ctx context.Context, auth ReadContext, ownerID string, through int, modeFilter ModeFilter) (Slice, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}

	var rows *sql.Rows
	var err error
	if ownerID == "" {
		rows, err = s.db.QueryContext(ctx,
			"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ordinal <= ? ORDER BY path_ref, path_ordinal ASC",
			through,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ref = ? AND path_ordinal <= ? ORDER BY path_ordinal ASC",
			ownerID, through,
		)
	}
	if err != nil {
		return Slice{}, fmt.Errorf("query owner prefix: %w", err)
	}
	defer rows.Close()

	var pathEntries []pathEntry
	for rows.Next() {
		var pe pathEntry
		if err := rows.Scan(&pe.recordID, &pe.pathRef, &pe.pathOrdinal); err != nil {
			return Slice{}, fmt.Errorf("scan path entry: %w", err)
		}
		pathEntries = append(pathEntries, pe)
	}

	return s.buildSlice(ctx, pathEntries, nil, opCtx.VisibilityProfile, modeFilter, true)
}

// ReadPathPrefix is the TraceStore protocol's path-addressed prefix read.
//
// In shepherd2 this is a pure alias for read_owner_prefix — the protocol declares
// it (facts.py:443) but the reference implementation's body is
// `return self.read_owner_prefix(read_context, trace_owner_id, through,
// mode_filter)`. It takes a trace owner id, not a free-form path, and applies no
// different addressing, so this delegates rather than reimplementing the query.
//
// It exists for protocol completeness: a caller written against the Python
// protocol can call either name and get the same slice.
func (s *SQLiteTraceStore) ReadPathPrefix(ctx context.Context, auth ReadContext, pathRef string, through int, modeFilter ModeFilter) (Slice, error) {
	return s.ReadOwnerPrefix(ctx, auth, pathRef, through, modeFilter)
}

// ReadCausalClosure reads the causal closure for one or more root facts.
func (s *SQLiteTraceStore) ReadCausalClosure(ctx context.Context, auth ReadContext, roots []string, modeFilter ModeFilter, closurePolicy string) (Slice, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}
	if err := ensureClosurePolicy(closurePolicy); err != nil {
		return Slice{}, err
	}

	// BFS over causal edges
	pending := make([]string, len(roots))
	copy(pending, roots)
	seen := make(map[string]bool)
	for len(pending) > 0 {
		factID := pending[0]
		pending = pending[1:]
		if seen[factID] {
			continue
		}
		seen[factID] = true

		fact, err := s.readFact(ctx, factID)
		if err != nil {
			return Slice{}, err
		}
		for _, parent := range fact.GetEnvelope().CausedByIDs {
			if !seen[parent] {
				pending = append(pending, parent)
			}
		}
	}

	entries := s.canonicalFactOrder(ctx, seen)
	includeAnchors := closurePolicy == "include_external_anchors"
	return s.buildSlice(ctx, entries, nil, opCtx.VisibilityProfile, modeFilter, includeAnchors)
}

// PublishFrontier publishes a retained owner-prefix frontier through the append path.
func (s *SQLiteTraceStore) PublishFrontier(ctx context.Context, auth AppendContext, spec FrontierSpec) (Frontier, error) {
	opCtx := auth.ToOperationContext(OpPublishCut)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return Frontier{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return Frontier{}, fmt.Errorf("begin transaction: %w", err)
	}

	frontier, err := s.publishFrontierInTx(tx, opCtx, spec)
	if err != nil {
		tx.Rollback()
		return Frontier{}, err
	}

	if err := tx.Commit(); err != nil {
		return Frontier{}, fmt.Errorf("commit: %w", err)
	}
	return frontier, nil
}

// PublishCut is an alias for PublishFrontier.
func (s *SQLiteTraceStore) PublishCut(ctx context.Context, auth AppendContext, spec FrontierSpec) (Frontier, error) {
	return s.PublishFrontier(ctx, auth, spec)
}

// ResolveFrontier resolves a frontier into a graph-shaped trace slice.
func (s *SQLiteTraceStore) ResolveFrontier(ctx context.Context, auth ReadContext, frontierID string, modeFilter ModeFilter) (Slice, error) {
	opCtx := auth.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}

	frontier, err := s.readOwnerCutoff(ctx, frontierID)
	if err != nil {
		return Slice{}, err
	}

	through, err := s.readFactAtPath(ctx, frontier.ThroughFactID, frontier.TargetTraceOwnerID, frontier.ThroughOwnerOrdinal)
	if err != nil {
		return Slice{}, err
	}
	if through.View == nil || through.View.TraceOwnerID != frontier.TargetTraceOwnerID {
		return Slice{}, &TraceStoreError{"frontier through fact owner disagrees with target trace owner"}
	}
	if through.View.OwnerOrdinal != frontier.ThroughOwnerOrdinal {
		return Slice{}, &TraceStoreError{"frontier through fact ordinal changed"}
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ref = ? AND path_ordinal <= ? ORDER BY path_ordinal ASC",
		frontier.TargetTraceOwnerID, frontier.ThroughOwnerOrdinal,
	)
	if err != nil {
		return Slice{}, fmt.Errorf("query frontier entries: %w", err)
	}
	defer rows.Close()

	var pathEntries []pathEntry
	for rows.Next() {
		var pe pathEntry
		if err := rows.Scan(&pe.recordID, &pe.pathRef, &pe.pathOrdinal); err != nil {
			return Slice{}, fmt.Errorf("scan path entry: %w", err)
		}
		pathEntries = append(pathEntries, pe)
	}

	return s.buildSlice(ctx, pathEntries, &frontier, opCtx.VisibilityProfile, modeFilter, true)
}

// ResolveCut is an alias for ResolveFrontier.
func (s *SQLiteTraceStore) ResolveCut(ctx context.Context, auth ReadContext, cutID string, modeFilter ModeFilter) (Slice, error) {
	return s.ResolveFrontier(ctx, auth, cutID, modeFilter)
}

// ReadOwnerCutoff reads a published frontier by ID.
func (s *SQLiteTraceStore) ReadOwnerCutoff(ctx context.Context, frontierID string) (Frontier, error) {
	return s.readOwnerCutoff(ctx, frontierID)
}

// FactCount returns the retained fact count for diagnostics.
func (s *SQLiteTraceStore) FactCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM records").Scan(&count)
	return count, err
}

// ContextCount returns the retained context count for diagnostics.
func (s *SQLiteTraceStore) ContextCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM contexts").Scan(&count)
	return count, err
}

// --- Internal types ---

type pathEntry struct {
	recordID    string
	pathRef     string
	pathOrdinal int
}

type witnessPlan struct {
	recordID   string
	schemaRef  string
	kindLabel  string
	body       map[string]any
	witnessRef string
}

// --- Schema creation ---

func (s *SQLiteTraceStore) createSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		INSERT OR IGNORE INTO meta(key, value) VALUES ('next_commit_seq', '0');

		CREATE TABLE IF NOT EXISTS append_intents (
			append_intent_id TEXT PRIMARY KEY,
			batch_digest TEXT NOT NULL,
			receipt_json TEXT NOT NULL,
			committed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS materialization_intents (
			materialize_intent_id TEXT PRIMARY KEY,
			request_digest TEXT NOT NULL,
			receipt_json TEXT NOT NULL,
			completed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS contexts (
			context_id TEXT PRIMARY KEY,
			active_binding_refs_json TEXT NOT NULL,
			capability_witness_refs_json TEXT NOT NULL,
			semantic_environment_refs_json TEXT NOT NULL,
			visibility_policy_refs_json TEXT NOT NULL,
			substrate_ref TEXT NOT NULL,
			containment TEXT NOT NULL,
			append_intent_id TEXT NOT NULL,
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE TABLE IF NOT EXISTS owner_ordinals (
			trace_owner_id TEXT PRIMARY KEY,
			next_ordinal INTEGER NOT NULL CHECK(next_ordinal >= 0)
		);

		CREATE TABLE IF NOT EXISTS records (
			record_id TEXT PRIMARY KEY,
			digest TEXT NOT NULL,
			schema_ref TEXT NOT NULL,
			mode TEXT NOT NULL,
			witness_ref TEXT NOT NULL,
			caused_by_json TEXT NOT NULL,
			body_json TEXT NOT NULL,
			append_intent_id TEXT NOT NULL,
			CHECK(record_id = digest),
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE TABLE IF NOT EXISTS record_edges (
			parent_record_id TEXT NOT NULL,
			child_record_id TEXT NOT NULL,
			parent_position INTEGER NOT NULL CHECK(parent_position >= 0),
			PRIMARY KEY(child_record_id, parent_position),
			FOREIGN KEY(parent_record_id) REFERENCES records(record_id),
			FOREIGN KEY(child_record_id) REFERENCES records(record_id)
		);

		CREATE INDEX IF NOT EXISTS idx_record_edges_parent ON record_edges(parent_record_id);

		CREATE TABLE IF NOT EXISTS path_entries (
			path_ref TEXT NOT NULL,
			path_ordinal INTEGER NOT NULL CHECK(path_ordinal >= 0),
			record_id TEXT NOT NULL,
			retained_context_ref TEXT NOT NULL,
			kind_label TEXT NOT NULL,
			append_intent_id TEXT NOT NULL,
			commit_receipt TEXT NOT NULL UNIQUE,
			PRIMARY KEY(path_ref, path_ordinal),
			FOREIGN KEY(record_id) REFERENCES records(record_id),
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);

		CREATE INDEX IF NOT EXISTS idx_path_entries_record ON path_entries(record_id);

		CREATE TABLE IF NOT EXISTS frontiers (
			frontier_id TEXT PRIMARY KEY,
			target_trace_owner_id TEXT NOT NULL,
			through_fact_id TEXT NOT NULL,
			through_owner_ordinal INTEGER NOT NULL CHECK(through_owner_ordinal >= 0),
			publisher_trace_owner_id TEXT,
			created_by_fact_id TEXT NOT NULL UNIQUE,
			append_intent_id TEXT NOT NULL UNIQUE,
			FOREIGN KEY(through_fact_id) REFERENCES records(record_id),
			FOREIGN KEY(created_by_fact_id) REFERENCES records(record_id),
			FOREIGN KEY(append_intent_id) REFERENCES append_intents(append_intent_id)
		);
	`)
	return err
}

// --- Append ---

func (s *SQLiteTraceStore) appendInTx(tx *sql.Tx, ctx OperationContext, batch AppendBatch) (AppendReceipt, bool, error) {
	if err := validateBatchShape(batch, ctx); err != nil {
		return AppendReceipt{}, false, err
	}
	batchDigest, err := batchDigest(batch, ctx)
	if err != nil {
		return AppendReceipt{}, false, err
	}

	// Check idempotency
	var existingDigest string
	var existingReceiptJSON string
	err = tx.QueryRow(
		"SELECT batch_digest, receipt_json FROM append_intents WHERE append_intent_id = ?",
		batch.AppendIntentID,
	).Scan(&existingDigest, &existingReceiptJSON)
	if err == nil {
		if existingDigest != batchDigest && existingDigest != legacyBatchDigest(batch, ctx) {
			return AppendReceipt{}, false, &AppendIntentConflictError{
				fmt.Sprintf("append intent %q was already committed with different content", batch.AppendIntentID),
			}
		}
		receipt, err := receiptFromJSON(existingReceiptJSON)
		return receipt, false, err // false = idempotent, no new records
	}
	if err != sql.ErrNoRows {
		return AppendReceipt{}, false, fmt.Errorf("query append intent: %w", err)
	}

	// Prepare append
	contexts, witnessPlans, facts, receipt, err := s.prepareAppend(tx, batch, ctx)
	if err != nil {
		return AppendReceipt{}, false, err
	}

	receiptJSON, err := receiptToJSON(receipt)
	if err != nil {
		return AppendReceipt{}, false, err
	}

	_, err = tx.Exec(
		"INSERT INTO append_intents(append_intent_id, batch_digest, receipt_json) VALUES (?, ?, ?)",
		batch.AppendIntentID, batchDigest, receiptJSON,
	)
	if err != nil {
		return AppendReceipt{}, false, fmt.Errorf("insert append intent: %w", err)
	}

	for _, ctx := range contexts {
		if err := s.insertContextTx(tx, ctx, batch.AppendIntentID); err != nil {
			return AppendReceipt{}, false, err
		}
	}

	for _, plan := range witnessPlans {
		if err := s.insertWitnessRecordIfMissingTx(tx, plan, batch.AppendIntentID); err != nil {
			return AppendReceipt{}, false, err
		}
	}

	for i, fact := range facts {
		if err := s.insertFactRowTx(tx, fact, receipt.CommitReceipts[i], batch.AppendIntentID); err != nil {
			return AppendReceipt{}, false, err
		}
	}

	for owner, rng := range receipt.OwnerRanges {
		_, err := tx.Exec(
			`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
			 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
			owner, rng[1]+1,
		)
		if err != nil {
			return AppendReceipt{}, false, fmt.Errorf("update owner ordinal: %w", err)
		}
	}

	if len(receipt.CommitReceipts) > 0 {
		nextSeq := nextCommitSeq(receipt.CommitReceipts)
		if _, err := tx.Exec("UPDATE meta SET value = ? WHERE key = 'next_commit_seq'", fmt.Sprintf("%d", nextSeq)); err != nil {
			return AppendReceipt{}, false, fmt.Errorf("update commit seq: %w", err)
		}
	}

	return receipt, true, nil // true = new append
}

func (s *SQLiteTraceStore) prepareAppend(tx *sql.Tx, batch AppendBatch, ctx OperationContext) (
	contexts []RetainedContext,
	witnessPlans []witnessPlan,
	facts []Record,
	receipt AppendReceipt,
	err error,
) {
	stagedIDs := make(map[string]bool)
	localFactIDs := make(map[string]string)
	stagedNext := make(map[string]int)

	// Resolve starting ordinals
	for _, group := range batch.Groups {
		owner := groupOwner(group)
		if _, ok := stagedNext[owner]; !ok {
			ord, err := s.nextOwnerOrdinalTx(tx, owner)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}
			stagedNext[owner] = ord
		}
	}

	var commitReceipts []string
	var causalEdges [][2]string
	var contextReceipts []string
	ownerRanges := make(map[string][2]int)
	witnessPlanMap := make(map[string]witnessPlan)
	var witnessPlanOrder []string

	// addWitnessPlan registers a plan, keeping first-seen order. Ranging over the
	// map to build the insert list, as this did, made the witness records' owner
	// ordinals depend on Go map iteration — so two identical appends could allocate
	// different ordinals, and the witness owner path read back in a different order
	// every time. Python's dict is insertion-ordered, so it never does.
	addWitnessPlan := func(plan witnessPlan) {
		if _, ok := witnessPlanMap[plan.recordID]; !ok {
			witnessPlanOrder = append(witnessPlanOrder, plan.recordID)
		}
		witnessPlanMap[plan.recordID] = plan
	}

	nextCommit, err := s.nextCommitSeqTx(tx)
	if err != nil {
		return nil, nil, nil, AppendReceipt{}, err
	}

	for groupIndex, group := range batch.Groups {
		owner := groupOwner(group)
		drafts := group.FactDrafts
		if len(drafts) == 0 {
			continue
		}

		// Resolve context
		retCtx, err := s.resolveGroupContextTx(tx, batch.AppendIntentID, groupIndex, group, ctx)
		if err != nil {
			return nil, nil, nil, AppendReceipt{}, err
		}
		if !s.contextExistsTx(tx, retCtx.ContextID) {
			contexts = append(contexts, retCtx)
		}
		contextReceipts = append(contextReceipts, retCtx.ContextID)

		// Resolve witness.
		//
		// The root witness is registered before this group's ordinary one, and both
		// keep first-seen order below. Python holds these in a dict and uses
		// setdefault in that same order (trace_store.py:480-481), and the order is
		// load-bearing: witnesses are inserted in it and that assignment is what
		// gives them their owner ordinals.
		addWitnessPlan(rootWitnessPlan())

		wp, err := ordinaryWitnessPlan(retCtx, ctx)
		if err != nil {
			return nil, nil, nil, AppendReceipt{}, err
		}
		addWitnessPlan(wp)

		start := stagedNext[owner]
		ordinal := start

		for _, draft := range drafts {
			causedBy, err := resolvedCauses(group, draft, localFactIDs)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}
			if err := s.validateCausalParentsTx(tx, causedBy, stagedIDs); err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}

			commitReceipt := fmt.Sprintf("commit:%d", nextCommit+len(facts))
			schemaRef, err := resolveSchemaRef(draft, ctx)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}

			factID, err := RecordDigest(schemaRef, draft.Mode, draft.Payload, causedBy, wp.recordID)
			if err != nil {
				return nil, nil, nil, AppendReceipt{}, err
			}

			if draft.AppendLocalID != "" {
				localFactIDs[draft.AppendLocalID] = factID
			}

			envelope := RecordEnvelope{
				RecordID:    factID,
				Digest:      factID,
				SchemaRef:   schemaRef,
				Mode:        draft.Mode,
				WitnessRef:  wp.recordID,
				CausedByIDs: causedBy,
			}
			kindLabel := draft.KindLabel
			if kindLabel == "" {
				kindLabel = schemaRef
			}
			view := &RecordView{
				TraceOwnerID: owner,
				OwnerOrdinal: ordinal,
				ContextRef:   retCtx.ContextID,
				KindLabel:    kindLabel,
			}
			body := RecordBody{Payload: copyMap(draft.Payload)}
			fact := Record{Envelope: envelope, Body: body, View: view}

			facts = append(facts, fact)
			commitReceipts = append(commitReceipts, commitReceipt)

			for _, parent := range causedBy {
				causalEdges = append(causalEdges, [2]string{parent, factID})
			}

			stagedIDs[factID] = true
			ordinal++
		}

		stagedNext[owner] = ordinal
		rng, ok := ownerRanges[owner]
		if !ok {
			ownerRanges[owner] = [2]int{start, ordinal - 1}
		} else {
			ownerRanges[owner] = [2]int{rng[0], ordinal - 1}
		}
	}

	// Collect witness plans in first-seen order, which becomes their insertion
	// order and therefore their owner ordinals.
	for _, id := range witnessPlanOrder {
		if wp, ok := witnessPlanMap[id]; ok {
			witnessPlans = append(witnessPlans, wp)
		}
	}

	receipt = AppendReceipt{
		AppendIntentID:  batch.AppendIntentID,
		FactIDs:         factIDs(facts),
		CommitReceipts:  commitReceipts,
		OwnerRanges:     ownerRanges,
		CausalEdges:     causalEdges,
		ContextReceipts: contextReceipts,
	}
	return contexts, witnessPlans, facts, receipt, nil
}

func (s *SQLiteTraceStore) previewFactIDs(gctx context.Context, batch AppendBatch, ctx OperationContext) ([]string, error) {
	// Use the same context resolution as prepareAppend
	localFactIDs := make(map[string]string)
	var factIDs []string

	for groupIndex, group := range batch.Groups {
		drafts := group.FactDrafts
		if len(drafts) == 0 {
			continue
		}

		// Use same context resolution as prepareAppend
		retCtx, err := s.resolveGroupContext(gctx, batch.AppendIntentID, groupIndex, group, ctx)
		if err != nil {
			return nil, err
		}
		wp, err := ordinaryWitnessPlan(retCtx, ctx)
		if err != nil {
			return nil, err
		}

		for _, draft := range drafts {
			causedBy, err := resolvedCauses(group, draft, localFactIDs)
			if err != nil {
				return nil, err
			}
			schemaRef, err := resolveSchemaRef(draft, ctx)
			if err != nil {
				return nil, err
			}
			factID, err := RecordDigest(schemaRef, draft.Mode, draft.Payload, causedBy, wp.recordID)
			if err != nil {
				return nil, err
			}
			if draft.AppendLocalID != "" {
				localFactIDs[draft.AppendLocalID] = factID
			}
			factIDs = append(factIDs, factID)
		}
	}
	return factIDs, nil
}

// --- Read ---

func (s *SQLiteTraceStore) readFact(gctx context.Context, factID string) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM records
		JOIN path_entries ON path_entries.record_id = records.record_id
		WHERE records.record_id = ?
		ORDER BY path_entries.path_ref ASC, path_entries.path_ordinal ASC
		LIMIT 1`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readFactAtPath(gctx context.Context, factID, ownerID string, ordinal int) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ? AND path_entries.path_ordinal = ?`,
		factID, ownerID, ordinal)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readRecord(gctx context.Context, factID string) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, '' AS trace_owner_id, -1 AS owner_ordinal,
		       '' AS retained_context_ref, '' AS kind_label
		FROM records WHERE records.record_id = ?`, factID)
	return scanRecord(row)
}

// --- Frontier ---

func (s *SQLiteTraceStore) publishFrontierInTx(tx *sql.Tx, ctx OperationContext, spec FrontierSpec) (Frontier, error) {
	if spec.FrontierID == "" {
		return Frontier{}, fmt.Errorf("frontier_id is required")
	}

	through, err := s.readLatestFactOnPathTx(tx, spec.ThroughFactID, spec.TargetTraceOwnerID)
	if err != nil {
		return Frontier{}, err
	}
	if through.View == nil || through.View.TraceOwnerID != spec.TargetTraceOwnerID {
		return Frontier{}, &TraceStoreError{"owner cutoff target disagrees with through fact owner"}
	}

	publisher := spec.PublisherOwnerID
	if publisher == "" {
		publisher = spec.TargetTraceOwnerID
	}
	throughOrdinal := through.View.OwnerOrdinal
	intent := spec.AppendIntentID
	if intent == "" {
		intent = fmt.Sprintf("frontier:%s", spec.FrontierID)
	}

	payload := map[string]any{
		"frontier_id":              spec.FrontierID,
		"target_trace_owner_id":    spec.TargetTraceOwnerID,
		"through_fact_id":          spec.ThroughFactID,
		"through_owner_ordinal":    throughOrdinal,
		"publisher_trace_owner_id": publisher,
	}

	causal := make([]string, 0, 1+len(spec.CausedBy))
	causal = append(causal, spec.ThroughFactID)
	causal = append(causal, spec.CausedBy...)

	retCtx := RetainedContext{
		ContextID:               fmt.Sprintf("ctx:%s:frontier", intent),
		CapabilityWitnessRefs:   ctx.PresentedAuthorityRefs,
		SemanticEnvironmentRefs: []string{ctx.SchemaEnvironmentRef},
		SubstrateRef:            "sqlite.local.v1",
		Containment:             ContainContained,
	}

	causedByFactIDs := make([]string, len(causal))
	copy(causedByFactIDs, causal)

	batch := AppendBatch{
		AppendIntentID: intent,
		Groups: []AppendGroup{{
			TraceOwnerID:    publisher,
			RetainedContext: &retCtx,
			CausalParents:   causedByFactIDs,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: "shepherd2.frontier.owner_cutoff.v1",
				KindLabel: "frontier_published",
				Payload:   payload,
			}},
		}},
	}

	receipt, _, err := s.appendInTx(tx, ctx, batch)
	if err != nil {
		return Frontier{}, err
	}

	createdByFactID := receipt.FactIDs[0]
	frontier := Frontier{
		FrontierID:          spec.FrontierID,
		TargetTraceOwnerID:  spec.TargetTraceOwnerID,
		ThroughFactID:       spec.ThroughFactID,
		ThroughOwnerOrdinal: throughOrdinal,
		PublisherOwnerID:    publisher,
		CreatedByFactID:     createdByFactID,
	}

	// Check if frontier already exists
	existingFrontier, err := s.readOwnerCutoffTx(tx, spec.FrontierID)
	if err == nil {
		if existingFrontier.FrontierID != frontier.FrontierID ||
			existingFrontier.TargetTraceOwnerID != frontier.TargetTraceOwnerID ||
			existingFrontier.ThroughFactID != frontier.ThroughFactID ||
			existingFrontier.ThroughOwnerOrdinal != frontier.ThroughOwnerOrdinal {
			return Frontier{}, &TraceStoreError{fmt.Sprintf("frontier id %q already names a different cutoff", spec.FrontierID)}
		}
		return existingFrontier, nil
	}

	// Insert frontier
	_, err = tx.Exec(`
		INSERT INTO frontiers(frontier_id, target_trace_owner_id, through_fact_id, through_owner_ordinal,
			publisher_trace_owner_id, created_by_fact_id, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		frontier.FrontierID, frontier.TargetTraceOwnerID, frontier.ThroughFactID,
		frontier.ThroughOwnerOrdinal, frontier.PublisherOwnerID, frontier.CreatedByFactID, intent,
	)
	if err != nil {
		return Frontier{}, fmt.Errorf("insert frontier: %w", err)
	}

	return frontier, nil
}

func (s *SQLiteTraceStore) readOwnerCutoff(gctx context.Context, frontierID string) (Frontier, error) {
	row := s.db.QueryRowContext(gctx, "SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	var f Frontier
	var publisherID, appendIntentID string
	err := row.Scan(&f.FrontierID, &f.TargetTraceOwnerID, &f.ThroughFactID,
		&f.ThroughOwnerOrdinal, &publisherID, &f.CreatedByFactID, &appendIntentID)
	if err == sql.ErrNoRows {
		return Frontier{}, &unknownFrontierError{frontierID: frontierID}
	}
	if err != nil {
		return Frontier{}, fmt.Errorf("scan frontier: %w", err)
	}
	f.PublisherOwnerID = publisherID

	// Verify against the retained frontier fact
	frontierFact, err := s.readFact(gctx, f.CreatedByFactID)
	if err != nil {
		return Frontier{}, err
	}

	payload := frontierFact.Body.Payload
	expected, err := frontierFromPayload(payload, f.CreatedByFactID)
	if err != nil {
		return Frontier{}, err
	}

	if f.FrontierID != expected.FrontierID ||
		f.TargetTraceOwnerID != expected.TargetTraceOwnerID ||
		f.ThroughFactID != expected.ThroughFactID ||
		f.ThroughOwnerOrdinal != expected.ThroughOwnerOrdinal {
		return Frontier{}, &TraceStoreError{"resolver index disagrees with retained frontier fact"}
	}

	return f, nil
}

// frontierFromPayload extracts a Frontier from a retained frontier fact's
// payload map. Returns an error if any required field is missing or has
// the wrong type — never silently uses a zero value.
func frontierFromPayload(payload map[string]any, createdByFactID string) (Frontier, error) {
	frontierID, ok := payload["frontier_id"].(string)
	if !ok || frontierID == "" {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid frontier_id"}
	}
	targetOwner, ok := payload["target_trace_owner_id"].(string)
	if !ok || targetOwner == "" {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid target_trace_owner_id"}
	}
	throughFact, ok := payload["through_fact_id"].(string)
	if !ok || throughFact == "" {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid through_fact_id"}
	}
	throughOrdinal, err := intFromPayload(payload, "through_owner_ordinal")
	if err != nil {
		return Frontier{}, err
	}
	publisher, _ := payload["publisher_trace_owner_id"].(string) // optional

	return Frontier{
		FrontierID:          frontierID,
		TargetTraceOwnerID:  targetOwner,
		ThroughFactID:       throughFact,
		ThroughOwnerOrdinal: throughOrdinal,
		PublisherOwnerID:    publisher,
		CreatedByFactID:     createdByFactID,
	}, nil
}

// intFromPayload reads an integer field from a decoded payload.
//
// The value may be a json.Number, because bodies are decoded with UseNumber to
// preserve the int/float distinction the canonical writer needs, or a float64
// from a decoder that was not. Rejecting either would silently read an ordinal as
// zero.
func intFromPayload(payload map[string]any, key string) (int, error) {
	switch v := payload[key].(type) {
	case json.Number:
		n, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return 0, &TraceStoreError{"frontier fact missing or invalid " + key}
		}
		return int(n), nil
	case float64:
		return int(v), nil
	default:
		return 0, &TraceStoreError{"frontier fact missing or invalid " + key}
	}
}

func (s *SQLiteTraceStore) readFrontierRow(gctx context.Context, frontierID string) (*sql.Row, error) {
	row := s.db.QueryRowContext(gctx, "SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	return row, nil
}

func (s *SQLiteTraceStore) readLatestFactOnPath(gctx context.Context, factID, ownerID string) (Record, error) {
	row := s.db.QueryRowContext(gctx, `
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ?
		ORDER BY path_entries.path_ordinal DESC LIMIT 1`, factID, ownerID)
	return scanRecord(row)
}

// --- Slice building ---

func (s *SQLiteTraceStore) buildSlice(
	gctx context.Context,
	pathEntries []pathEntry,
	frontier *Frontier,
	visibility VisibilityProfile,
	modeFilter ModeFilter,
	includeExternalAnchors bool,
) (Slice, error) {
	// Filter by mode
	var loaded []struct {
		entry pathEntry
		fact  Record
	}
	for _, pe := range pathEntries {
		fact, err := s.readFactAtPath(gctx, pe.recordID, pe.pathRef, pe.pathOrdinal)
		if err != nil {
			return Slice{}, err
		}
		if modeMatches(fact, modeFilter) {
			loaded = append(loaded, struct {
				entry pathEntry
				fact  Record
			}{pe, fact})
		}
	}

	selected := make(map[string]bool)
	for _, l := range loaded {
		selected[l.entry.recordID] = true
	}

	// Owner paths and the three anchor collections are maps, but their order is
	// part of the read result. Python's TraceSlice holds all four as dicts and
	// emits them in insertion order — dict order is insertion order — so
	// fact_ids() and the anchor tuples are deterministic there. Go's map
	// iteration is randomised, so each map is paired with the order its keys were
	// first inserted and emitted through orderedValues. Iterating these maps
	// directly makes every read produce a different order: different from Python,
	// and different from the previous call in the same process.
	factsByID := make(map[string]VisibleRecord)
	contextsByID := make(map[string]RetainedContext)
	ownerPaths := make(map[string][]string)
	var ownerPathOrder []string
	var causalEdges [][2]string
	externalAnchors := make(map[string]ExternalAnchor)
	var externalAnchorOrder []string
	contextAnchors := make(map[string]ContextAnchor)
	var contextAnchorOrder []string
	witnessesByID := make(map[string]VisibleRecord)
	witnessAnchors := make(map[string]WitnessAnchor)
	var witnessAnchorOrder []string

	for _, l := range loaded {
		factID := l.entry.recordID
		visible := visibleFact(l.fact, visibility)
		if visible != nil {
			factsByID[factID] = visible
		}
		if _, seen := ownerPaths[l.entry.pathRef]; !seen {
			ownerPathOrder = append(ownerPathOrder, l.entry.pathRef)
		}
		ownerPaths[l.entry.pathRef] = append(ownerPaths[l.entry.pathRef], factID)

		for _, parent := range l.fact.Envelope.CausedByIDs {
			if selected[parent] {
				causalEdges = append(causalEdges, [2]string{parent, factID})
			} else if includeExternalAnchors {
				if _, exists := externalAnchors[parent]; !exists {
					externalAnchorOrder = append(externalAnchorOrder, parent)
					externalAnchors[parent] = s.anchorForFact(gctx, parent, "outside_frontier")
				}
			}
		}

		ctxID := ""
		if l.fact.View != nil {
			ctxID = l.fact.View.ContextRef
		}
		if ctxID != "" {
			if visibility == VisibilityShapeOnly {
				if _, exists := contextAnchors[ctxID]; !exists {
					contextAnchorOrder = append(contextAnchorOrder, ctxID)
					contextAnchors[ctxID] = ContextAnchor{
						ContextID:    ctxID,
						VisibleShape: map[string]any{"context_id": ctxID},
					}
				}
			} else {
				if _, exists := contextsByID[ctxID]; !exists {
					ctx, err := s.readContext(gctx, ctxID)
					if err == nil {
						contextsByID[ctxID] = ctx
					}
				}
			}
		}
	}

	// Witness support closure
	var loadedRecords []Record
	for _, l := range loaded {
		loadedRecords = append(loadedRecords, l.fact)
	}
	witnessSupport, err := s.witnessSupportClosure(gctx, loadedRecords)
	if err != nil {
		return Slice{}, err
	}
	for _, w := range witnessSupport {
		wRef := w.Envelope.RecordID
		visible := visibleFact(w, visibility)
		if visibility == VisibilityShapeOnly {
			if _, exists := witnessAnchors[wRef]; !exists {
				witnessAnchorOrder = append(witnessAnchorOrder, wRef)
				witnessAnchors[wRef] = witnessAnchor(w)
			}
		} else if visible != nil {
			witnessesByID[wRef] = visible
		}
	}

	return Slice{
		Frontier:          frontier,
		VisibilityProfile: visibility,
		ModeFilter:        modeFilter,
		FactsByID:         factsByID,
		ContextsByID:      contextsByID,
		OwnerPaths:        ownerPaths,
		OwnerPathOrder:    ownerPathOrder,
		CausalEdges:       causalEdges,
		ExternalAnchors:   orderedValues(externalAnchors, externalAnchorOrder),
		ContextAnchors:    orderedValues(contextAnchors, contextAnchorOrder),
		WitnessesByID:     witnessesByID,
		WitnessAnchors:    orderedValues(witnessAnchors, witnessAnchorOrder),
	}, nil
}

func (s *SQLiteTraceStore) anchorForFact(gctx context.Context, factID, hiddenReason string) ExternalAnchor {
	fact, err := s.readFact(gctx, factID)
	if err != nil {
		return ExternalAnchor{Ref: factID, AnchorKind: externalAnchorKindFact, HiddenReason: "unknown"}
	}
	kindLabel := ""
	if fact.View != nil {
		kindLabel = fact.View.KindLabel
	}
	traceOwnerID := ""
	ownerOrdinal := -1
	if fact.View != nil {
		traceOwnerID = fact.View.TraceOwnerID
		ownerOrdinal = fact.View.OwnerOrdinal
	}
	return ExternalAnchor{
		Ref:        factID,
		AnchorKind: externalAnchorKindFact,
		VisibleShape: map[string]any{
			"kind_label":     kindLabel,
			"schema_ref":     fact.Envelope.SchemaRef,
			"trace_owner_id": traceOwnerID,
			"owner_ordinal":  ownerOrdinal,
			"witness_ref":    fact.Envelope.WitnessRef,
		},
		HiddenReason: hiddenReason,
	}
}

// witnessSupport accumulates the witness records a slice depends on, remembering
// the order each was first encountered.
//
// The order matters and is not incidental: it becomes Slice.WitnessAnchors, and
// Python builds the equivalent structure as a dict — insertion-ordered — then
// converts it to a tuple. Returning these records from a bare map range, as this
// did, made the anchor order differ on every read.
type witnessSupport struct {
	byID  map[string]Record
	order []string
}

func newWitnessSupport() *witnessSupport {
	return &witnessSupport{byID: make(map[string]Record)}
}

func (w *witnessSupport) add(rec Record) {
	id := rec.Envelope.RecordID
	if _, exists := w.byID[id]; !exists {
		w.order = append(w.order, id)
	}
	w.byID[id] = rec
}

func (w *witnessSupport) get(id string) (Record, bool) {
	rec, ok := w.byID[id]
	return rec, ok
}

// records returns the witnesses in first-seen order.
func (w *witnessSupport) records() []Record {
	out := make([]Record, 0, len(w.byID))
	for _, id := range w.order {
		if rec, ok := w.byID[id]; ok {
			out = append(out, rec)
		}
	}
	return out
}

func (s *SQLiteTraceStore) witnessSupportClosure(gctx context.Context, records []Record) ([]Record, error) {
	support := newWitnessSupport()
	validatedToRoot := make(map[string]bool)

	for _, record := range records {
		if record.Envelope.WitnessRef == "" && record.Envelope.SchemaRef != RootWitnessSchemaRef {
			return nil, &TraceStoreError{fmt.Sprintf("non-root record has empty witness_ref: %s", record.Envelope.RecordID)}
		}
	}

	for _, record := range records {
		if record.Envelope.WitnessRef != "" {
			if err := s.validateWitnessChain(gctx, record.Envelope.WitnessRef, support, validatedToRoot); err != nil {
				return nil, err
			}
		}
	}

	return support.records(), nil
}

func (s *SQLiteTraceStore) validateWitnessChain(gctx context.Context, startRef string, support *witnessSupport, validatedToRoot map[string]bool) error {
	seenInChain := make(map[string]bool)
	witnessRef := startRef

	for {
		if validatedToRoot[witnessRef] {
			return nil
		}
		if seenInChain[witnessRef] {
			return &TraceStoreError{fmt.Sprintf("witness chain cycle before root witness: %s", witnessRef)}
		}
		seenInChain[witnessRef] = true

		witness, ok := support.get(witnessRef)
		if !ok {
			var err error
			witness, err = s.readFact(gctx, witnessRef)
			if err != nil {
				return &TraceStoreError{fmt.Sprintf("witness ref does not resolve: %s", witnessRef)}
			}
			if witness.Envelope.SchemaRef != RootWitnessSchemaRef && witness.Envelope.SchemaRef != WitnessSchemaRef {
				return &TraceStoreError{fmt.Sprintf("witness_ref does not resolve to a witness record: %s", witnessRef)}
			}
			support.add(witness)
		}

		if witness.Envelope.SchemaRef != RootWitnessSchemaRef && witness.Envelope.SchemaRef != WitnessSchemaRef {
			return &TraceStoreError{fmt.Sprintf("witness_ref does not resolve to a witness record: %s", witnessRef)}
		}
		if witness.Envelope.SchemaRef == RootWitnessSchemaRef {
			if witness.Envelope.WitnessRef != RootWitnessRef {
				return &TraceStoreError{"root witness record must use the empty witness sentinel"}
			}
			for ref := range seenInChain {
				validatedToRoot[ref] = true
			}
			return nil
		}
		if witness.Envelope.WitnessRef == "" {
			return &TraceStoreError{fmt.Sprintf("non-root witness has empty witness_ref: %s", witnessRef)}
		}
		witnessRef = witness.Envelope.WitnessRef
	}
}

func (s *SQLiteTraceStore) canonicalFactOrder(gctx context.Context, factIDs map[string]bool) []pathEntry {
	if len(factIDs) == 0 {
		return nil
	}

	ids := make([]any, 0, len(factIDs))
	for id := range factIDs {
		ids = append(ids, id)
	}
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}

	query := fmt.Sprintf(`
		SELECT record_id, path_ref, path_ordinal FROM path_entries
		WHERE record_id IN (%s)
		ORDER BY path_ref ASC, path_ordinal ASC, record_id ASC`,
		strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(gctx, query, ids...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// The entries are collected in the query's ORDER BY order, not through a
	// map: ranging a map discards the sort and randomizes the slice on every
	// call, which is what happened before this was pinned — a closure read
	// returned its facts in a different order on each process run, while a
	// single run always looked correct. A record reachable on several paths
	// keeps its first row, which the SQL order makes deterministic too.
	seen := make(map[string]bool)
	result := make([]pathEntry, 0, len(ids))
	for rows.Next() {
		var pe pathEntry
		if err := rows.Scan(&pe.recordID, &pe.pathRef, &pe.pathOrdinal); err != nil {
			continue
		}
		if !seen[pe.recordID] {
			seen[pe.recordID] = true
			result = append(result, pe)
		}
	}
	return result
}

// --- Context management ---

func (s *SQLiteTraceStore) resolveGroupContext(
	gctx context.Context, appendIntentID string, groupIndex int, group AppendGroup, ctx OperationContext,
) (RetainedContext, error) {
	if group.RetainedContext != nil && group.RetainedContext.ContextID != "" {
		// Check if this is a reuse request
		if group.RetainedContext.SubstrateRef == "" {
			// It's a reuse reference
			return s.readContext(gctx, group.RetainedContext.ContextID)
		}
	}

	draft := defaultContextDraft()
	if group.RetainedContext != nil {
		draft = *group.RetainedContext
	}

	payload := contextPayload(draft, ctx)
	contextID, err := contextIDFor(appendIntentID, groupIndex, payload)
	if err != nil {
		return RetainedContext{}, err
	}

	newCtx := RetainedContext{
		ContextID:               contextID,
		ActiveBindingRefs:       payload.ActiveBindingRefs,
		CapabilityWitnessRefs:   payload.CapabilityWitnessRefs,
		SemanticEnvironmentRefs: payload.SemanticEnvironmentRefs,
		VisibilityPolicyRefs:    payload.VisibilityPolicyRefs,
		SubstrateRef:            payload.SubstrateRef,
		Containment:             payload.Containment,
	}

	existing, err := s.readContext(gctx, contextID)
	if err == nil {
		if !contextEqual(existing, newCtx) {
			return RetainedContext{}, &TraceStoreError{fmt.Sprintf("context id %q already names a different context", contextID)}
		}
		return existing, nil
	}

	return newCtx, nil
}

func (s *SQLiteTraceStore) contextExists(gctx context.Context, contextID string) bool {
	var count int
	s.db.QueryRowContext(gctx, "SELECT 1 FROM contexts WHERE context_id = ?", contextID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) readContext(gctx context.Context, contextID string) (RetainedContext, error) {
	row := s.db.QueryRowContext(gctx, "SELECT * FROM contexts WHERE context_id = ?", contextID)
	var ctx RetainedContext
	var activeJSON, capJSON, semJSON, visJSON string
	err := row.Scan(&ctx.ContextID, &activeJSON, &capJSON, &semJSON, &visJSON,
		&ctx.SubstrateRef, &ctx.Containment, new(string))
	if err == sql.ErrNoRows {
		return RetainedContext{}, &TraceStoreError{fmt.Sprintf("unknown context id: %s", contextID)}
	}
	if err != nil {
		return RetainedContext{}, fmt.Errorf("scan context: %w", err)
	}
	ctx.ActiveBindingRefs = jsonToStrings(activeJSON)
	ctx.CapabilityWitnessRefs = jsonToStrings(capJSON)
	ctx.SemanticEnvironmentRefs = jsonToStrings(semJSON)
	ctx.VisibilityPolicyRefs = jsonToStrings(visJSON)
	return ctx, nil
}

func (s *SQLiteTraceStore) insertContext(gctx context.Context, rc RetainedContext, appendIntentID string) error {
	_, err := s.db.ExecContext(gctx, `
		INSERT INTO contexts(context_id, active_binding_refs_json, capability_witness_refs_json,
			semantic_environment_refs_json, visibility_policy_refs_json, substrate_ref, containment, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rc.ContextID,
		stringToJSON(rc.ActiveBindingRefs),
		stringToJSON(rc.CapabilityWitnessRefs),
		stringToJSON(rc.SemanticEnvironmentRefs),
		stringToJSON(rc.VisibilityPolicyRefs),
		rc.SubstrateRef,
		string(rc.Containment),
		appendIntentID,
	)
	return err
}

// --- Witness management ---

func (s *SQLiteTraceStore) insertWitnessRecordIfMissing(gctx context.Context, plan witnessPlan, appendIntentID string) error {
	if s.factExists(gctx, plan.recordID) {
		existing, err := s.readFact(gctx, plan.recordID)
		if err != nil {
			return err
		}
		if !witnessFactMatchesPlan(existing, plan) {
			return &TraceStoreError{fmt.Sprintf("witness record id %q already names a different witness", plan.recordID)}
		}
		return nil
	}

	ordinal, err := s.nextOwnerOrdinal(gctx, witnessTraceOwnerID)
	if err != nil {
		return err
	}

	fact := Record{
		Envelope: RecordEnvelope{
			RecordID:   plan.recordID,
			Digest:     plan.recordID,
			SchemaRef:  plan.schemaRef,
			Mode:       Capture,
			WitnessRef: plan.witnessRef,
		},
		Body: RecordBody{Payload: copyMap(plan.body)},
		View: &RecordView{
			TraceOwnerID: witnessTraceOwnerID,
			OwnerOrdinal: ordinal,
			KindLabel:    plan.kindLabel,
		},
	}

	if err := s.insertFactRow(gctx, fact, fmt.Sprintf("witness:%s", plan.recordID), appendIntentID); err != nil {
		return err
	}

	_, err = s.db.ExecContext(gctx,
		`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
		 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
		witnessTraceOwnerID, ordinal+1,
	)
	return err
}

// --- Record insertion ---

func (s *SQLiteTraceStore) insertFactRow(gctx context.Context, fact Record, commitReceipt, appendIntentID string) error {
	if fact.View == nil {
		return &TraceStoreError{"path append requires record view metadata"}
	}
	if err := s.insertRecordIfMissing(gctx, fact, appendIntentID); err != nil {
		return err
	}

	_, err := s.db.ExecContext(gctx, `
		INSERT INTO path_entries(path_ref, path_ordinal, record_id, retained_context_ref, kind_label, append_intent_id, commit_receipt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fact.View.TraceOwnerID,
		fact.View.OwnerOrdinal,
		fact.Envelope.RecordID,
		fact.View.ContextRef,
		fact.View.KindLabel,
		appendIntentID,
		commitReceipt,
	)
	return err
}

func (s *SQLiteTraceStore) insertRecordIfMissing(gctx context.Context, fact Record, appendIntentID string) error {
	if fact.Envelope.RecordID != fact.Envelope.Digest {
		return &TraceStoreError{"record_id must equal digest"}
	}
	if s.factExists(gctx, fact.Envelope.RecordID) {
		existing, err := s.readRecord(gctx, fact.Envelope.RecordID)
		if err != nil {
			return err
		}
		if !recordContentMatches(existing, fact) {
			return &TraceStoreError{fmt.Sprintf("record id %q already names different content", fact.Envelope.RecordID)}
		}
		return nil
	}

	_, err := s.db.ExecContext(gctx, `
		INSERT INTO records(record_id, digest, schema_ref, mode, witness_ref, caused_by_json, body_json, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.Envelope.RecordID,
		fact.Envelope.Digest,
		fact.Envelope.SchemaRef,
		string(fact.Envelope.Mode),
		fact.Envelope.WitnessRef,
		stringToJSON(fact.Envelope.CausedByIDs),
		bodyToJSON(fact.Body),
		appendIntentID,
	)
	if err != nil {
		return err
	}

	for i, parent := range fact.Envelope.CausedByIDs {
		_, err := s.db.ExecContext(gctx,
			"INSERT INTO record_edges(parent_record_id, child_record_id, parent_position) VALUES (?, ?, ?)",
			parent, fact.Envelope.RecordID, i,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SQLiteTraceStore) factExists(gctx context.Context, factID string) bool {
	var count int
	s.db.QueryRowContext(gctx, "SELECT 1 FROM records WHERE record_id = ?", factID).Scan(&count)
	return count > 0
}

// --- Ordinal management ---

func (s *SQLiteTraceStore) nextOwnerOrdinal(gctx context.Context, ownerID string) (int, error) {
	var ordinal int
	err := s.db.QueryRowContext(gctx, "SELECT next_ordinal FROM owner_ordinals WHERE trace_owner_id = ?", ownerID).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query owner ordinal: %w", err)
	}
	return ordinal, nil
}

func (s *SQLiteTraceStore) nextCommitSeq(gctx context.Context) (int, error) {
	var seq int
	err := s.db.QueryRowContext(gctx, "SELECT value FROM meta WHERE key = 'next_commit_seq'").Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("query commit seq: %w", err)
	}
	return seq, nil
}

// --- Helpers ---

func scanRecord(row *sql.Row) (Record, error) {
	var r Record
	var causedByJSON, bodyJSON string
	var traceOwnerID, contextRef, kindLabel string
	var ownerOrdinal int

	err := row.Scan(
		&r.Envelope.RecordID, &r.Envelope.Digest, &r.Envelope.SchemaRef,
		&r.Envelope.Mode, &r.Envelope.WitnessRef, &causedByJSON, &bodyJSON,
		new(string), // append_intent_id
		&traceOwnerID, &ownerOrdinal, &contextRef, &kindLabel,
	)
	if err == sql.ErrNoRows {
		return Record{}, &UnknownFactError{"unknown fact"}
	}
	if err != nil {
		return Record{}, fmt.Errorf("scan record: %w", err)
	}

	r.Envelope.CausedByIDs = jsonToStrings(causedByJSON)
	body, err := bodyFromJSON(bodyJSON)
	if err != nil {
		return Record{}, fmt.Errorf("record %s: %w", r.Envelope.RecordID, err)
	}
	r.Body = body
	r.View = &RecordView{
		TraceOwnerID: traceOwnerID,
		OwnerOrdinal: ownerOrdinal,
		ContextRef:   contextRef,
		KindLabel:    kindLabel,
	}
	return r, nil
}

func factIDs(facts []Record) []string {
	ids := make([]string, len(facts))
	for i, f := range facts {
		ids[i] = f.Envelope.RecordID
	}
	return ids
}

func groupOwner(group AppendGroup) string {
	return group.TraceOwnerID
}

func validateBatchShape(batch AppendBatch, ctx OperationContext) error {
	if batch.AppendIntentID == "" {
		return fmt.Errorf("append_intent_id is required")
	}
	seenLocalRefs := make(map[string]bool)
	for _, group := range batch.Groups {
		if group.TraceOwnerID == "" {
			return fmt.Errorf("trace_owner_id is required")
		}
		for _, draft := range group.FactDrafts {
			if draft.Mode != Capture && draft.Mode != Declaration {
				return fmt.Errorf("fact mode must be 'capture' or 'declaration'")
			}
			if draft.AppendLocalID != "" {
				if seenLocalRefs[draft.AppendLocalID] {
					return fmt.Errorf("duplicate append-local id: %s", draft.AppendLocalID)
				}
				seenLocalRefs[draft.AppendLocalID] = true
			}
			if _, err := resolveSchemaRef(draft, ctx); err != nil {
				return err
			}
			if _, err := json.Marshal(draft.Payload); err != nil {
				return fmt.Errorf("payload not JSON-serializable: %w", err)
			}
		}
	}
	return nil
}

func resolvedCauses(group AppendGroup, draft RecordDraft, localFactIDs map[string]string) ([]string, error) {
	var local []string
	for _, ref := range draft.CausedByLocalRefs {
		factID, ok := localFactIDs[ref]
		if !ok {
			return nil, &TraceStoreError{fmt.Sprintf("append-local causal parent does not exist: %s", ref)}
		}
		local = append(local, factID)
	}

	// Duplicates across the merged parent list are rejected, not deduplicated:
	// the same parent cited at both the group and the draft level is a caller
	// error, and deduping it would change the parent tuple that the record
	// digest — and therefore the record id — is taken over. Python rejects it
	// with ValueError("duplicate causal parent"); so does this.
	seen := make(map[string]bool)
	var result []string
	addParent := func(id string) error {
		if seen[id] {
			return fmt.Errorf("duplicate causal parent: %s", id)
		}
		seen[id] = true
		result = append(result, id)
		return nil
	}
	for _, id := range group.CausalParents {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	for _, id := range draft.CausedByFactIDs {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	for _, id := range local {
		if err := addParent(id); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *SQLiteTraceStore) validateCausalParents(gctx context.Context, causedBy []string, stagedIDs map[string]bool) error {
	for _, id := range causedBy {
		if stagedIDs[id] {
			continue
		}
		if !s.factExists(gctx, id) {
			return &UnknownFactError{fmt.Sprintf("causal parent does not exist: %s", id)}
		}
	}
	return nil
}

func resolveSchemaRef(draft RecordDraft, ctx OperationContext) (string, error) {
	if draft.SchemaRef != "" {
		return draft.SchemaRef, nil
	}
	return "", fmt.Errorf("schema_ref is required")
}

func defaultContextDraft() RetainedContext {
	return RetainedContext{
		SubstrateRef: "sqlite.local.v1",
		Containment:  ContainContained,
	}
}

func contextPayload(draft RetainedContext, ctx OperationContext) RetainedContext {
	semEnv := append([]string{}, draft.SemanticEnvironmentRefs...)
	semEnv = append(semEnv, ctx.SchemaEnvironmentRef)
	semEnv = deduplicate(semEnv)

	return RetainedContext{
		ActiveBindingRefs:       append([]string{}, draft.ActiveBindingRefs...),
		CapabilityWitnessRefs:   append([]string{}, draft.CapabilityWitnessRefs...),
		SemanticEnvironmentRefs: semEnv,
		VisibilityPolicyRefs:    append([]string{}, draft.VisibilityPolicyRefs...),
		SubstrateRef:            draft.SubstrateRef,
		Containment:             draft.Containment,
	}
}

// contextIDFor derives a retained context id the way shepherd2's `_context_id`
// does:
//
//	digest = sha256(f"{intent}\0{group_index}\0{json}").hexdigest()
//	return f"context:{digest[:32]}"
//
// where `json` is the context payload on its own — not wrapped in an object — and
// is encoded with ensure_ascii=True. All three details were wrong before:
//
//   - the input is a NUL-joined *string*, not a JSON object of intent/group/payload;
//   - the result is the first 32 hex characters with a "context:" prefix, not all
//     64 with "ctx:";
//   - the encoder is the ASCII flavour, because `_json_dumps` omits ensure_ascii.
//
// With an ASCII-only payload the last point is invisible, which is why this
// survived: the ids were simply always different, and nothing compared them.
func contextIDFor(appendIntentID string, groupIndex int, payload RetainedContext) (string, error) {
	body, err := canonicalJSONASCIIBytes(map[string]any{
		"active_binding_refs":       payload.ActiveBindingRefs,
		"capability_witness_refs":   payload.CapabilityWitnessRefs,
		"semantic_environment_refs": payload.SemanticEnvironmentRefs,
		"visibility_policy_refs":    payload.VisibilityPolicyRefs,
		"substrate_ref":             payload.SubstrateRef,
		"containment":               string(payload.Containment),
	})
	if err != nil {
		return "", fmt.Errorf("containerd context id: %w", err)
	}
	joined := append([]byte(appendIntentID), 0)
	joined = strconv.AppendInt(joined, int64(groupIndex), 10)
	joined = append(joined, 0)
	joined = append(joined, body...)

	digest := fmt.Sprintf("%x", sha256Sum(joined))
	return "context:" + digest[:32], nil
}

func contextEqual(a, b RetainedContext) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}

// ordinaryWitnessPlan builds the witness a group's records cite.
//
// authority_refs comes from the *retained context's* capability_witness_refs, not
// from the append context's presented refs. Python does the same
// (trace_store.py `_ordinary_witness_plan`), and getting it wrong ties the witness
// id — and therefore every record id that cites it — to the caller's credentials:
// the same content appended by a different trusted caller produced a different
// record id, which Python does not do.
func ordinaryWitnessPlan(ctx RetainedContext, opCtx OperationContext) (witnessPlan, error) {
	body := map[string]any{
		"actor_ref":                 opCtx.ActorRef,
		"authority_refs":            nonNilStrings(ctx.CapabilityWitnessRefs),
		"active_binding_refs":       nonNilStrings(ctx.ActiveBindingRefs),
		"semantic_environment_refs": nonNilStrings(ctx.SemanticEnvironmentRefs),
		"visibility_policy_refs":    nonNilStrings(ctx.VisibilityPolicyRefs),
		"provenance_policy_refs":    []string{},
		"substrate_ref":             ctx.SubstrateRef,
		"containment":               string(ctx.Containment),
	}
	// The witness body is validated before it is digested, so a group context
	// with an empty substrate_ref or an unknown containment fails the append —
	// and the preview, which builds the same plan. Python validates in
	// _ordinary_witness_plan; without this the store would retain a witness
	// whose body fails validate_witness_body, and the record citing it would
	// digest over an input the kernel's own schema rejects.
	if err := ValidateWitnessBody(WitnessSchemaRef, body); err != nil {
		return witnessPlan{}, err
	}
	rootID := RootWitnessRecordIDMust()
	recordID, err := RecordDigest(WitnessSchemaRef, Capture, body, nil, rootID)
	if err != nil {
		return witnessPlan{}, fmt.Errorf("ordinary witness digest: %w", err)
	}
	return witnessPlan{
		recordID:   recordID,
		schemaRef:  WitnessSchemaRef,
		kindLabel:  "witness",
		body:       body,
		witnessRef: rootID,
	}, nil
}

// nonNilStrings returns an empty (non-nil) slice for nil input.
//
// The canonical writer emits null for a nil slice and [] for an empty one, and
// Python serialises an empty tuple as []. Without this a Go zero value would
// canonicalise differently from the Python it is supposed to match.
func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func rootWitnessPlan() witnessPlan {
	body := RootWitnessBody()
	recordID := RootWitnessRecordIDMust()
	return witnessPlan{
		recordID:  recordID,
		schemaRef: RootWitnessSchemaRef,
		// Python uses "witness_root" (trace_store.py `_root_witness_plan`); this
		// said "root_witness", which is retained metadata a read would expose.
		kindLabel:  "witness_root",
		body:       body,
		witnessRef: RootWitnessRef,
	}
}

func witnessFactMatchesPlan(fact Record, plan witnessPlan) bool {
	return fact.Envelope.SchemaRef == plan.schemaRef &&
		fact.Envelope.Mode == Capture &&
		fact.Envelope.WitnessRef == plan.witnessRef
}

func recordContentMatches(a, b Record) bool {
	aJSON, _ := json.Marshal(map[string]any{
		"schema_ref":  a.Envelope.SchemaRef,
		"mode":        string(a.Envelope.Mode),
		"witness_ref": a.Envelope.WitnessRef,
		"caused_by":   a.Envelope.CausedByIDs,
		"body":        a.Body.Payload,
	})
	bJSON, _ := json.Marshal(map[string]any{
		"schema_ref":  b.Envelope.SchemaRef,
		"mode":        string(b.Envelope.Mode),
		"witness_ref": b.Envelope.WitnessRef,
		"caused_by":   b.Envelope.CausedByIDs,
		"body":        b.Body.Payload,
	})
	return string(aJSON) == string(bJSON)
}

func visibleFact(fact Record, visibility VisibilityProfile) VisibleRecord {
	switch visibility {
	case VisibilityPayload, VisibilityFullInternal:
		return fact
	case VisibilityShapeOnly:
		return RecordShape{
			Envelope:     fact.Envelope,
			View:         fact.View,
			HiddenReason: "payload_hidden",
		}
	default:
		return fact
	}
}

func witnessAnchor(w Record) WitnessAnchor {
	return WitnessAnchor{
		WitnessRef: w.Envelope.RecordID,
		VisibleShape: map[string]any{
			"schema_ref":  w.Envelope.SchemaRef,
			"witness_ref": w.Envelope.WitnessRef,
		},
		HiddenReason: "hidden_by_visibility",
	}
}

func modeMatches(fact Record, modeFilter ModeFilter) bool {
	switch modeFilter {
	case ModeBoth:
		return true
	case ModeDeclarationsOnly:
		return fact.Envelope.Mode == Declaration
	case ModeCapturesOnly:
		return fact.Envelope.Mode == Capture
	default:
		return true
	}
}

// legacyBatchDigest reproduces the pre-correction digest algorithm, which
// marshalled Go structs with encoding/json.
//
// It exists only to read rows written before the algorithm was aligned with
// Python, and is never stored. Without it, a database created by an earlier
// release would see every retry of an already-committed intent as a *different*
// batch — because the stored digest cannot be recomputed — and return
// AppendIntentConflictError instead of the stored receipt. That would break
// idempotent retry, which is the property the check exists to provide.
//
// It is safe to keep: accepting either digest means an old row can be read, while
// a row written now can only match the corrected value. Retire it when no
// supported database predates the correction.
func legacyBatchDigest(batch AppendBatch, ctx OperationContext) string {
	data := map[string]any{
		"append_intent_id": batch.AppendIntentID,
		"groups":           batch.Groups,
		"actor_ref":        ctx.ActorRef,
		"authority_refs":   ctx.PresentedAuthorityRefs,
		"schema_env":       ctx.SchemaEnvironmentRef,
		"trust_mode":       ctx.TrustMode,
	}
	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256Sum(b))
}

// nullableString renders an unset Go string as JSON null, which is what Python's
// Optional fields serialise to. Go has no separate "unset" for a string field.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// contextDraftJSON renders a group's retained-context reference the way Python's
// `_context_draft_to_json` does, including `RetainedContextDraft`'s defaults.
//
// Python distinguishes three cases in `_context_draft_for_group` — an explicit
// draft, a reuse reference by context id, or nothing at all — and the "nothing"
// case still carries the defaults (substrate_ref "sqlite.local.v1", containment
// "contained"). Go represents all three with one pointer, so which one it is has to
// be inferred the same way the rest of the store infers it: an id with no substrate
// ref is a reuse reference.
func contextDraftJSON(rc *RetainedContext) map[string]any {
	defaults := defaultContextDraft()
	draft := map[string]any{
		"active_binding_refs":       []string{},
		"capability_witness_refs":   []string{},
		"semantic_environment_refs": []string{},
		"visibility_policy_refs":    []string{},
		"substrate_ref":             defaults.SubstrateRef,
		"containment":               string(defaults.Containment),
		"reuse_context_id":          nil,
	}
	if rc == nil {
		return draft
	}
	if rc.ContextID != "" && rc.SubstrateRef == "" {
		draft["reuse_context_id"] = rc.ContextID
		return draft
	}
	draft["active_binding_refs"] = nonNilStrings(rc.ActiveBindingRefs)
	draft["capability_witness_refs"] = nonNilStrings(rc.CapabilityWitnessRefs)
	draft["semantic_environment_refs"] = nonNilStrings(rc.SemanticEnvironmentRefs)
	draft["visibility_policy_refs"] = nonNilStrings(rc.VisibilityPolicyRefs)
	draft["substrate_ref"] = rc.SubstrateRef
	draft["containment"] = string(rc.Containment)
	return draft
}

// batchDigest fingerprints an append batch for "same intent, different content"
// detection, matching shepherd2's `_batch_digest` field for field.
//
// It previously marshalled Go structs directly, so the JSON carried Go field names
// and declaration order rather than the sorted snake_case keys Python hashes. The
// two could never agree, and that matters beyond bookkeeping: the conflict check is
// part of the ABI, so a batch one store rejects as a conflicting reuse the other
// would accept.
func batchDigest(batch AppendBatch, ctx OperationContext) (string, error) {
	groups := make([]any, 0, len(batch.Groups))
	for _, g := range batch.Groups {
		drafts := make([]any, 0, len(g.FactDrafts))
		for _, d := range g.FactDrafts {
			schemaRef, err := resolveSchemaRef(d, ctx)
			if err != nil {
				return "", err
			}
			drafts = append(drafts, map[string]any{
				"append_local_id":      nullableString(d.AppendLocalID),
				"kind_label":           d.KindLabel,
				"mode":                 string(d.Mode),
				"schema_ref":           schemaRef,
				"payload":              d.Payload,
				"caused_by_fact_ids":   nonNilStrings(d.CausedByFactIDs),
				"caused_by_local_refs": nonNilStrings(d.CausedByLocalRefs),
			})
		}
		groups = append(groups, map[string]any{
			"trace_owner_id":   g.TraceOwnerID,
			"retained_context": contextDraftJSON(g.RetainedContext),
			"causal_parents":   nonNilStrings(g.CausalParents),
			"fact_drafts":      drafts,
		})
	}

	payload := map[string]any{
		"append_intent_id": batch.AppendIntentID,
		// Python's AppendBatch carries an atomicity field defaulting to "atomic";
		// the Go type only supports atomic batches, so it is fixed here.
		"atomicity":          "atomic",
		"actor_ref":          ctx.ActorRef,
		"schema_version_set": ctx.SchemaEnvironmentRef,
		"groups":             groups,
	}

	b, err := canonicalJSONASCIIBytes(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256Sum(b)), nil
}

func receiptToJSON(r AppendReceipt) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func receiptFromJSON(s string) (AppendReceipt, error) {
	var r AppendReceipt
	err := json.Unmarshal([]byte(s), &r)
	return r, err
}

func bodyToJSON(body RecordBody) string {
	b, _ := json.Marshal(body.Payload)
	return string(b)
}

// bodyFromJSON decodes a retained body.
//
// It uses UseNumber because the canonical writer depends on the int/float
// distinction that JSON carries in the token text: `1` and `1.0` are different
// values to Python and canonicalise to "1" and "1.0". With the default decoder
// every number becomes float64, so a retained body that said `1` would
// re-canonicalise as `1.0` and the record could no longer reproduce its own id —
// which is exactly what happened before this was fixed.
func bodyFromJSON(s string) (RecordBody, error) {
	var payload map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		// Reported rather than swallowed. An empty body would still be returned to
		// the caller looking like a valid record whose payload happened to be
		// empty, and any digest computed from it would be a plausible wrong answer.
		return RecordBody{}, fmt.Errorf("decode retained body: %w", err)
	}
	return RecordBody{Payload: payload}, nil
}

func stringToJSON(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}

func jsonToStrings(s string) []string {
	var result []string
	json.Unmarshal([]byte(s), &result)
	return result
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func deduplicate(ss []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

func nextCommitSeq(receipts []string) int {
	maxSeq := 0
	for _, r := range receipts {
		var seq int
		fmt.Sscanf(r, "commit:%d", &seq)
		if seq >= maxSeq {
			maxSeq = seq + 1
		}
	}
	return maxSeq
}

// orderedValues returns a map's values in the given key order.
//
// It exists because every map-backed field of a read result has a defined order
// in the Python reference — dicts are insertion-ordered — while Go's map
// iteration is randomised. Callers pass the insertion order they recorded.
//
// The length check is a guard, not an expected path: if the supplied order does
// not cover the map, this falls back to sorted keys rather than to map iteration,
// so a missed insertion site degrades to a different deterministic order instead
// of to nondeterminism.
func orderedValues[T any](m map[string]T, order []string) []T {
	if len(order) != len(m) {
		order = make([]string, 0, len(m))
		for k := range m {
			order = append(order, k)
		}
		sort.Strings(order)
	}
	s := make([]T, 0, len(m))
	for _, k := range order {
		if v, ok := m[k]; ok {
			s = append(s, v)
		}
	}
	return s
}

func (s *SQLiteTraceStore) validateCausalParentsTx(tx *sql.Tx, causedBy []string, stagedIDs map[string]bool) error {
	for _, id := range causedBy {
		if stagedIDs[id] {
			continue
		}
		if !s.factExistsTx(tx, id) {
			return &UnknownFactError{fmt.Sprintf("causal parent does not exist: %s", id)}
		}
	}
	return nil
}

func (s *SQLiteTraceStore) resolveGroupContextTx(
	tx *sql.Tx, appendIntentID string, groupIndex int, group AppendGroup, ctx OperationContext,
) (RetainedContext, error) {
	if group.RetainedContext != nil && group.RetainedContext.ContextID != "" {
		if group.RetainedContext.SubstrateRef == "" {
			return s.readContextTx(tx, group.RetainedContext.ContextID)
		}
	}

	draft := defaultContextDraft()
	if group.RetainedContext != nil {
		draft = *group.RetainedContext
	}

	payload := contextPayload(draft, ctx)
	contextID, err := contextIDFor(appendIntentID, groupIndex, payload)
	if err != nil {
		return RetainedContext{}, err
	}

	newCtx := RetainedContext{
		ContextID:               contextID,
		ActiveBindingRefs:       payload.ActiveBindingRefs,
		CapabilityWitnessRefs:   payload.CapabilityWitnessRefs,
		SemanticEnvironmentRefs: payload.SemanticEnvironmentRefs,
		VisibilityPolicyRefs:    payload.VisibilityPolicyRefs,
		SubstrateRef:            payload.SubstrateRef,
		Containment:             payload.Containment,
	}

	existing, err := s.readContextTx(tx, contextID)
	if err == nil {
		if !contextEqual(existing, newCtx) {
			return RetainedContext{}, &TraceStoreError{fmt.Sprintf("context id %q already names a different context", contextID)}
		}
		return existing, nil
	}

	return newCtx, nil
}

// --- Tx-aware helpers for transaction-internal operations ---

func (s *SQLiteTraceStore) readOwnerCutoffTx(tx *sql.Tx, frontierID string) (Frontier, error) {
	row := tx.QueryRow("SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	var f Frontier
	var publisherID, appendIntentID string
	err := row.Scan(&f.FrontierID, &f.TargetTraceOwnerID, &f.ThroughFactID,
		&f.ThroughOwnerOrdinal, &publisherID, &f.CreatedByFactID, &appendIntentID)
	if err == sql.ErrNoRows {
		return Frontier{}, &unknownFrontierError{frontierID: frontierID}
	}
	if err != nil {
		return Frontier{}, fmt.Errorf("scan frontier: %w", err)
	}
	f.PublisherOwnerID = publisherID
	return f, nil
}

func (s *SQLiteTraceStore) readLatestFactOnPathTx(tx *sql.Tx, factID, ownerID string) (Record, error) {
	row := tx.QueryRow(`
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ?
		ORDER BY path_entries.path_ordinal DESC LIMIT 1`, factID, ownerID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readFactTx(tx *sql.Tx, factID string) (Record, error) {
	row := tx.QueryRow(`
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM records
		JOIN path_entries ON path_entries.record_id = records.record_id
		WHERE records.record_id = ?
		ORDER BY path_entries.path_ref ASC, path_entries.path_ordinal ASC
		LIMIT 1`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readRecordTx(tx *sql.Tx, factID string) (Record, error) {
	row := tx.QueryRow(`
		SELECT records.*, '' AS trace_owner_id, -1 AS owner_ordinal,
		       '' AS retained_context_ref, '' AS kind_label
		FROM records WHERE records.record_id = ?`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) factExistsTx(tx *sql.Tx, factID string) bool {
	var count int
	tx.QueryRow("SELECT 1 FROM records WHERE record_id = ?", factID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) contextExistsTx(tx *sql.Tx, contextID string) bool {
	var count int
	tx.QueryRow("SELECT 1 FROM contexts WHERE context_id = ?", contextID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) readContextTx(tx *sql.Tx, contextID string) (RetainedContext, error) {
	row := tx.QueryRow("SELECT * FROM contexts WHERE context_id = ?", contextID)
	var ctx RetainedContext
	var activeJSON, capJSON, semJSON, visJSON string
	err := row.Scan(&ctx.ContextID, &activeJSON, &capJSON, &semJSON, &visJSON,
		&ctx.SubstrateRef, &ctx.Containment, new(string))
	if err == sql.ErrNoRows {
		return RetainedContext{}, &TraceStoreError{fmt.Sprintf("unknown context id: %s", contextID)}
	}
	if err != nil {
		return RetainedContext{}, fmt.Errorf("scan context: %w", err)
	}
	ctx.ActiveBindingRefs = jsonToStrings(activeJSON)
	ctx.CapabilityWitnessRefs = jsonToStrings(capJSON)
	ctx.SemanticEnvironmentRefs = jsonToStrings(semJSON)
	ctx.VisibilityPolicyRefs = jsonToStrings(visJSON)
	return ctx, nil
}

func (s *SQLiteTraceStore) nextOwnerOrdinalTx(tx *sql.Tx, ownerID string) (int, error) {
	var ordinal int
	err := tx.QueryRow("SELECT next_ordinal FROM owner_ordinals WHERE trace_owner_id = ?", ownerID).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query owner ordinal: %w", err)
	}
	return ordinal, nil
}

func (s *SQLiteTraceStore) nextCommitSeqTx(tx *sql.Tx) (int, error) {
	var seq int
	err := tx.QueryRow("SELECT value FROM meta WHERE key = 'next_commit_seq'").Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("query commit seq: %w", err)
	}
	return seq, nil
}

func (s *SQLiteTraceStore) insertContextTx(tx *sql.Tx, ctx RetainedContext, appendIntentID string) error {
	_, err := tx.Exec(`
		INSERT INTO contexts(context_id, active_binding_refs_json, capability_witness_refs_json,
			semantic_environment_refs_json, visibility_policy_refs_json, substrate_ref, containment, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ctx.ContextID,
		stringToJSON(ctx.ActiveBindingRefs),
		stringToJSON(ctx.CapabilityWitnessRefs),
		stringToJSON(ctx.SemanticEnvironmentRefs),
		stringToJSON(ctx.VisibilityPolicyRefs),
		ctx.SubstrateRef,
		string(ctx.Containment),
		appendIntentID,
	)
	return err
}

func (s *SQLiteTraceStore) insertWitnessRecordIfMissingTx(tx *sql.Tx, plan witnessPlan, appendIntentID string) error {
	if s.factExistsTx(tx, plan.recordID) {
		existing, err := s.readFactTx(tx, plan.recordID)
		if err != nil {
			return err
		}
		if !witnessFactMatchesPlan(existing, plan) {
			return &TraceStoreError{fmt.Sprintf("witness record id %q already names a different witness", plan.recordID)}
		}
		return nil
	}

	ordinal, err := s.nextOwnerOrdinalTx(tx, witnessTraceOwnerID)
	if err != nil {
		return err
	}

	fact := Record{
		Envelope: RecordEnvelope{
			RecordID:   plan.recordID,
			Digest:     plan.recordID,
			SchemaRef:  plan.schemaRef,
			Mode:       Capture,
			WitnessRef: plan.witnessRef,
		},
		Body: RecordBody{Payload: copyMap(plan.body)},
		View: &RecordView{
			TraceOwnerID: witnessTraceOwnerID,
			OwnerOrdinal: ordinal,
			KindLabel:    plan.kindLabel,
		},
	}

	if err := s.insertFactRowTx(tx, fact, fmt.Sprintf("witness:%s", plan.recordID), appendIntentID); err != nil {
		return err
	}

	_, err = tx.Exec(
		`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
		 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
		witnessTraceOwnerID, ordinal+1,
	)
	return err
}

func (s *SQLiteTraceStore) insertFactRowTx(tx *sql.Tx, fact Record, commitReceipt, appendIntentID string) error {
	if fact.View == nil {
		return &TraceStoreError{"path append requires record view metadata"}
	}
	if err := s.insertRecordIfMissingTx(tx, fact, appendIntentID); err != nil {
		return err
	}

	_, err := tx.Exec(`
		INSERT INTO path_entries(path_ref, path_ordinal, record_id, retained_context_ref, kind_label, append_intent_id, commit_receipt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fact.View.TraceOwnerID,
		fact.View.OwnerOrdinal,
		fact.Envelope.RecordID,
		fact.View.ContextRef,
		fact.View.KindLabel,
		appendIntentID,
		commitReceipt,
	)
	return err
}

func (s *SQLiteTraceStore) insertRecordIfMissingTx(tx *sql.Tx, fact Record, appendIntentID string) error {
	if fact.Envelope.RecordID != fact.Envelope.Digest {
		return &TraceStoreError{"record_id must equal digest"}
	}
	if s.factExistsTx(tx, fact.Envelope.RecordID) {
		existing, err := s.readRecordTx(tx, fact.Envelope.RecordID)
		if err != nil {
			return err
		}
		if !recordContentMatches(existing, fact) {
			return &TraceStoreError{fmt.Sprintf("record id %q already names different content", fact.Envelope.RecordID)}
		}
		return nil
	}

	_, err := tx.Exec(`
		INSERT INTO records(record_id, digest, schema_ref, mode, witness_ref, caused_by_json, body_json, append_intent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.Envelope.RecordID,
		fact.Envelope.Digest,
		fact.Envelope.SchemaRef,
		string(fact.Envelope.Mode),
		fact.Envelope.WitnessRef,
		stringToJSON(fact.Envelope.CausedByIDs),
		bodyToJSON(fact.Body),
		appendIntentID,
	)
	if err != nil {
		return err
	}

	for i, parent := range fact.Envelope.CausedByIDs {
		_, err := tx.Exec(
			"INSERT INTO record_edges(parent_record_id, child_record_id, parent_position) VALUES (?, ?, ?)",
			parent, fact.Envelope.RecordID, i,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// --- Authorization ---

func ensureAppendAuthorized(ctx OperationContext) error {
	if ctx.TrustMode == "internal" {
		return nil
	}
	for _, ref := range ctx.PresentedAuthorityRefs {
		if ref == "trusted:internal" {
			return nil
		}
	}
	return &TraceStoreError{"Slice A append requires trusted internal witness"}
}

func ensureReadAuthorized(ctx OperationContext) error {
	if ctx.VisibilityProfile != VisibilityShapeOnly &&
		ctx.VisibilityProfile != VisibilityPayload &&
		ctx.VisibilityProfile != VisibilityFullInternal {
		return fmt.Errorf("unknown visibility profile: %s", ctx.VisibilityProfile)
	}
	if ctx.VisibilityProfile == VisibilityFullInternal {
		trusted := false
		for _, ref := range ctx.PresentedAuthorityRefs {
			if ref == "trusted:internal" {
				trusted = true
				break
			}
		}
		if !trusted {
			return &TraceStoreError{"full_internal reads require trusted internal witness"}
		}
	}
	return nil
}

func ensureModeFilter(f ModeFilter) error {
	if f != ModeBoth && f != ModeDeclarationsOnly && f != ModeCapturesOnly {
		return fmt.Errorf("unknown mode filter: %s", f)
	}
	return nil
}

func ensureClosurePolicy(p string) error {
	if p != "visible_only" && p != "include_external_anchors" {
		return fmt.Errorf("unknown closure policy: %s", p)
	}
	return nil
}
