package shepherd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

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
func (s *SQLiteTraceStore) Close() error {
	return s.db.Close()
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

// q returns the transaction if non-nil, otherwise the raw db.
func (s *SQLiteTraceStore) q(tx *sql.Tx) queryer {
	if tx != nil {
		return tx
	}
	return s.db
}

// Append appends a semantic batch or returns the prior receipt for its intent.
func (s *SQLiteTraceStore) Append(ctx AppendContext, batch AppendBatch) (AppendReceipt, error) {
	opCtx := ctx.ToOperationContext(OpAppend)
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
func (s *SQLiteTraceStore) PreviewRecordIDs(ctx AppendContext, batch AppendBatch) ([]string, error) {
	opCtx := ctx.ToOperationContext(OpAppend)
	if err := ensureAppendAuthorized(opCtx); err != nil {
		return nil, err
	}
	if err := validateBatchShape(batch, opCtx); err != nil {
		return nil, err
	}
	return s.previewFactIDs(batch, opCtx)
}

// ReadFact reads one retained fact.
func (s *SQLiteTraceStore) ReadFact(ctx ReadContext, factID string) (VisibleRecord, error) {
	opCtx := ctx.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return nil, err
	}
	return s.readFact(factID)
}

// ReadOwnerPrefix reads all facts on an owner path up to and including the given ordinal.
func (s *SQLiteTraceStore) ReadOwnerPrefix(ctx ReadContext, ownerID string, through int, modeFilter ModeFilter) (Slice, error) {
	opCtx := ctx.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}

	var rows *sql.Rows
	var err error
	if ownerID == "" {
		rows, err = s.db.Query(
			"SELECT record_id, path_ref, path_ordinal FROM path_entries WHERE path_ordinal <= ? ORDER BY path_ref, path_ordinal ASC",
			through,
		)
	} else {
		rows, err = s.db.Query(
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

	return s.buildSlice(pathEntries, nil, opCtx.VisibilityProfile, modeFilter, true)
}

// ReadCausalClosure reads the causal closure for one or more root facts.
func (s *SQLiteTraceStore) ReadCausalClosure(ctx ReadContext, roots []string, modeFilter ModeFilter, closurePolicy string) (Slice, error) {
	opCtx := ctx.ToOperationContext()
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

		fact, err := s.readFact(factID)
		if err != nil {
			return Slice{}, err
		}
		for _, parent := range fact.GetEnvelope().CausedByIDs {
			if !seen[parent] {
				pending = append(pending, parent)
			}
		}
	}

	entries := s.canonicalFactOrder(seen)
	includeAnchors := closurePolicy == "include_external_anchors"
	return s.buildSlice(entries, nil, opCtx.VisibilityProfile, modeFilter, includeAnchors)
}

// PublishFrontier publishes a retained owner-prefix frontier through the append path.
func (s *SQLiteTraceStore) PublishFrontier(ctx AppendContext, spec FrontierSpec) (Frontier, error) {
	opCtx := ctx.ToOperationContext(OpPublishCut)
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
func (s *SQLiteTraceStore) PublishCut(ctx AppendContext, spec FrontierSpec) (Frontier, error) {
	return s.PublishFrontier(ctx, spec)
}

// ResolveFrontier resolves a frontier into a graph-shaped trace slice.
func (s *SQLiteTraceStore) ResolveFrontier(ctx ReadContext, frontierID string, modeFilter ModeFilter) (Slice, error) {
	opCtx := ctx.ToOperationContext()
	if err := ensureReadAuthorized(opCtx); err != nil {
		return Slice{}, err
	}
	if err := ensureModeFilter(modeFilter); err != nil {
		return Slice{}, err
	}

	frontier, err := s.readOwnerCutoff(frontierID)
	if err != nil {
		return Slice{}, err
	}

	through, err := s.readFactAtPath(frontier.ThroughFactID, frontier.TargetTraceOwnerID, frontier.ThroughOwnerOrdinal)
	if err != nil {
		return Slice{}, err
	}
	if through.View == nil || through.View.TraceOwnerID != frontier.TargetTraceOwnerID {
		return Slice{}, &TraceStoreError{"frontier through fact owner disagrees with target trace owner"}
	}
	if through.View.OwnerOrdinal != frontier.ThroughOwnerOrdinal {
		return Slice{}, &TraceStoreError{"frontier through fact ordinal changed"}
	}

	rows, err := s.db.Query(
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

	return s.buildSlice(pathEntries, &frontier, opCtx.VisibilityProfile, modeFilter, true)
}

// ResolveCut is an alias for ResolveFrontier.
func (s *SQLiteTraceStore) ResolveCut(ctx ReadContext, cutID string, modeFilter ModeFilter) (Slice, error) {
	return s.ResolveFrontier(ctx, cutID, modeFilter)
}

// ReadOwnerCutoff reads a published frontier by ID.
func (s *SQLiteTraceStore) ReadOwnerCutoff(frontierID string) (Frontier, error) {
	return s.readOwnerCutoff(frontierID)
}

// FactCount returns the retained fact count for diagnostics.
func (s *SQLiteTraceStore) FactCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM records").Scan(&count)
	return count, err
}

// ContextCount returns the retained context count for diagnostics.
func (s *SQLiteTraceStore) ContextCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM contexts").Scan(&count)
	return count, err
}

// --- Internal types ---

type pathEntry struct {
	recordID    string
	pathRef     string
	pathOrdinal int
}

type witnessPlan struct {
	recordID  string
	schemaRef string
	kindLabel string
	body      map[string]any
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
		if existingDigest != batchDigest {
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

		// Resolve witness
		wp := ordinaryWitnessPlan(retCtx, ctx)
		rootID := RootWitnessRecordIDMust()
		if _, ok := witnessPlanMap[rootID]; !ok {
			witnessPlanMap[rootID] = rootWitnessPlan()
		}
		witnessPlanMap[wp.recordID] = wp

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

	// Collect witness plans
	for _, wp := range witnessPlanMap {
		witnessPlans = append(witnessPlans, wp)
	}

	receipt = AppendReceipt{
		AppendIntentID:   batch.AppendIntentID,
		FactIDs:          factIDs(facts),
		CommitReceipts:   commitReceipts,
		OwnerRanges:      ownerRanges,
		CausalEdges:      causalEdges,
		ContextReceipts:  contextReceipts,
	}
	return contexts, witnessPlans, facts, receipt, nil
}

func (s *SQLiteTraceStore) previewFactIDs(batch AppendBatch, ctx OperationContext) ([]string, error) {
	// Use the same context resolution as prepareAppend
	localFactIDs := make(map[string]string)
	var factIDs []string

	for groupIndex, group := range batch.Groups {
		drafts := group.FactDrafts
		if len(drafts) == 0 {
			continue
		}

		// Use same context resolution as prepareAppend
		retCtx, err := s.resolveGroupContext(batch.AppendIntentID, groupIndex, group, ctx)
		if err != nil {
			return nil, err
		}
		wp := ordinaryWitnessPlan(retCtx, ctx)

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

func (s *SQLiteTraceStore) readFact(factID string) (Record, error) {
	row := s.db.QueryRow(`
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM records
		JOIN path_entries ON path_entries.record_id = records.record_id
		WHERE records.record_id = ?
		ORDER BY path_entries.path_ref ASC, path_entries.path_ordinal ASC
		LIMIT 1`, factID)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readFactAtPath(factID, ownerID string, ordinal int) (Record, error) {
	row := s.db.QueryRow(`
		SELECT records.*, path_entries.path_ref, path_entries.path_ordinal,
		       path_entries.retained_context_ref, path_entries.kind_label
		FROM path_entries
		JOIN records ON records.record_id = path_entries.record_id
		WHERE path_entries.record_id = ? AND path_entries.path_ref = ? AND path_entries.path_ordinal = ?`,
		factID, ownerID, ordinal)
	return scanRecord(row)
}

func (s *SQLiteTraceStore) readRecord(factID string) (Record, error) {
	row := s.db.QueryRow(`
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
		"frontier_id":             spec.FrontierID,
		"target_trace_owner_id":   spec.TargetTraceOwnerID,
		"through_fact_id":         spec.ThroughFactID,
		"through_owner_ordinal":   throughOrdinal,
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

func (s *SQLiteTraceStore) readOwnerCutoff(frontierID string) (Frontier, error) {
	row := s.db.QueryRow("SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	var f Frontier
	var publisherID, appendIntentID string
	err := row.Scan(&f.FrontierID, &f.TargetTraceOwnerID, &f.ThroughFactID,
		&f.ThroughOwnerOrdinal, &publisherID, &f.CreatedByFactID, &appendIntentID)
	if err == sql.ErrNoRows {
		return Frontier{}, &TraceStoreError{fmt.Sprintf("unknown frontier id: %q", frontierID)}
	}
	if err != nil {
		return Frontier{}, fmt.Errorf("scan frontier: %w", err)
	}
	f.PublisherOwnerID = publisherID

	// Verify against the retained frontier fact
	frontierFact, err := s.readFact(f.CreatedByFactID)
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
	throughOrdinalRaw, ok := payload["through_owner_ordinal"].(float64)
	if !ok {
		return Frontier{}, &TraceStoreError{"frontier fact missing or invalid through_owner_ordinal"}
	}
	publisher, _ := payload["publisher_trace_owner_id"].(string) // optional

	return Frontier{
		FrontierID:          frontierID,
		TargetTraceOwnerID:  targetOwner,
		ThroughFactID:       throughFact,
		ThroughOwnerOrdinal: int(throughOrdinalRaw),
		PublisherOwnerID:    publisher,
		CreatedByFactID:     createdByFactID,
	}, nil
}

func (s *SQLiteTraceStore) readFrontierRow(frontierID string) (*sql.Row, error) {
	row := s.db.QueryRow("SELECT * FROM frontiers WHERE frontier_id = ?", frontierID)
	return row, nil
}

func (s *SQLiteTraceStore) readLatestFactOnPath(factID, ownerID string) (Record, error) {
	row := s.db.QueryRow(`
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
		fact, err := s.readFactAtPath(pe.recordID, pe.pathRef, pe.pathOrdinal)
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

	factsByID := make(map[string]VisibleRecord)
	contextsByID := make(map[string]RetainedContext)
	ownerPaths := make(map[string][]string)
	var causalEdges [][2]string
	externalAnchors := make(map[string]ExternalAnchor)
	contextAnchors := make(map[string]ContextAnchor)
	witnessesByID := make(map[string]VisibleRecord)
	witnessAnchors := make(map[string]WitnessAnchor)

	for _, l := range loaded {
		factID := l.entry.recordID
		visible := visibleFact(l.fact, visibility)
		if visible != nil {
			factsByID[factID] = visible
		}
		ownerPaths[l.entry.pathRef] = append(ownerPaths[l.entry.pathRef], factID)

		for _, parent := range l.fact.Envelope.CausedByIDs {
			if selected[parent] {
				causalEdges = append(causalEdges, [2]string{parent, factID})
			} else if includeExternalAnchors {
				if _, exists := externalAnchors[parent]; !exists {
					externalAnchors[parent] = s.anchorForFact(parent, "outside_frontier")
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
					contextAnchors[ctxID] = ContextAnchor{
						ContextID:    ctxID,
						VisibleShape: map[string]any{"context_id": ctxID},
					}
				}
			} else {
				if _, exists := contextsByID[ctxID]; !exists {
					ctx, err := s.readContext(ctxID)
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
	witnessSupport, err := s.witnessSupportClosure(loadedRecords)
	if err != nil {
		return Slice{}, err
	}
	for _, w := range witnessSupport {
		wRef := w.Envelope.RecordID
		visible := visibleFact(w, visibility)
		if visibility == VisibilityShapeOnly {
			if _, exists := witnessAnchors[wRef]; !exists {
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
		CausalEdges:       causalEdges,
		ExternalAnchors:   mapToSlice(externalAnchors),
		ContextAnchors:    mapToSlice(contextAnchors),
		WitnessesByID:     witnessesByID,
		WitnessAnchors:    mapToSlice(witnessAnchors),
	}, nil
}

func (s *SQLiteTraceStore) anchorForFact(factID, hiddenReason string) ExternalAnchor {
	fact, err := s.readFact(factID)
	if err != nil {
		return ExternalAnchor{Ref: factID, HiddenReason: "unknown"}
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
		Ref: factID,
		VisibleShape: map[string]any{
			"kind_label":    kindLabel,
			"schema_ref":    fact.Envelope.SchemaRef,
			"trace_owner_id": traceOwnerID,
			"owner_ordinal": ownerOrdinal,
			"witness_ref":   fact.Envelope.WitnessRef,
		},
		HiddenReason: hiddenReason,
	}
}

func (s *SQLiteTraceStore) witnessSupportClosure(records []Record) ([]Record, error) {
	supportByID := make(map[string]Record)
	validatedToRoot := make(map[string]bool)

	for _, record := range records {
		if record.Envelope.WitnessRef == "" && record.Envelope.SchemaRef != RootWitnessSchemaRef {
			return nil, &TraceStoreError{fmt.Sprintf("non-root record has empty witness_ref: %s", record.Envelope.RecordID)}
		}
	}

	for _, record := range records {
		if record.Envelope.WitnessRef != "" {
			if err := s.validateWitnessChain(record.Envelope.WitnessRef, supportByID, validatedToRoot); err != nil {
				return nil, err
			}
		}
	}

	result := make([]Record, 0, len(supportByID))
	for _, w := range supportByID {
		result = append(result, w)
	}
	return result, nil
}

func (s *SQLiteTraceStore) validateWitnessChain(startRef string, supportByID map[string]Record, validatedToRoot map[string]bool) error {
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

		witness, ok := supportByID[witnessRef]
		if !ok {
			var err error
			witness, err = s.readFact(witnessRef)
			if err != nil {
				return &TraceStoreError{fmt.Sprintf("witness ref does not resolve: %s", witnessRef)}
			}
			if witness.Envelope.SchemaRef != RootWitnessSchemaRef && witness.Envelope.SchemaRef != WitnessSchemaRef {
				return &TraceStoreError{fmt.Sprintf("witness_ref does not resolve to a witness record: %s", witnessRef)}
			}
			supportByID[witnessRef] = witness
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

func (s *SQLiteTraceStore) canonicalFactOrder(factIDs map[string]bool) []pathEntry {
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

	rows, err := s.db.Query(query, ids...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	entriesByRecord := make(map[string]pathEntry)
	for rows.Next() {
		var pe pathEntry
		if err := rows.Scan(&pe.recordID, &pe.pathRef, &pe.pathOrdinal); err != nil {
			continue
		}
		if _, exists := entriesByRecord[pe.recordID]; !exists {
			entriesByRecord[pe.recordID] = pe
		}
	}

	result := make([]pathEntry, 0, len(entriesByRecord))
	for _, pe := range entriesByRecord {
		result = append(result, pe)
	}
	return result
}

// --- Context management ---

func (s *SQLiteTraceStore) resolveGroupContext(
	appendIntentID string, groupIndex int, group AppendGroup, ctx OperationContext,
) (RetainedContext, error) {
	if group.RetainedContext != nil && group.RetainedContext.ContextID != "" {
		// Check if this is a reuse request
		if group.RetainedContext.SubstrateRef == "" {
			// It's a reuse reference
			return s.readContext(group.RetainedContext.ContextID)
		}
	}

	draft := defaultContextDraft()
	if group.RetainedContext != nil {
		draft = *group.RetainedContext
	}

	payload := contextPayload(draft, ctx)
	contextID := contextIDFor(appendIntentID, groupIndex, payload)

	newCtx := RetainedContext{
		ContextID:               contextID,
		ActiveBindingRefs:       payload.ActiveBindingRefs,
		CapabilityWitnessRefs:   payload.CapabilityWitnessRefs,
		SemanticEnvironmentRefs: payload.SemanticEnvironmentRefs,
		VisibilityPolicyRefs:    payload.VisibilityPolicyRefs,
		SubstrateRef:            payload.SubstrateRef,
		Containment:             payload.Containment,
	}

	existing, err := s.readContext(contextID)
	if err == nil {
		if !contextEqual(existing, newCtx) {
			return RetainedContext{}, &TraceStoreError{fmt.Sprintf("context id %q already names a different context", contextID)}
		}
		return existing, nil
	}

	return newCtx, nil
}

func (s *SQLiteTraceStore) contextExists(contextID string) bool {
	var count int
	s.db.QueryRow("SELECT 1 FROM contexts WHERE context_id = ?", contextID).Scan(&count)
	return count > 0
}

func (s *SQLiteTraceStore) readContext(contextID string) (RetainedContext, error) {
	row := s.db.QueryRow("SELECT * FROM contexts WHERE context_id = ?", contextID)
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

func (s *SQLiteTraceStore) insertContext(ctx RetainedContext, appendIntentID string) error {
	_, err := s.db.Exec(`
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

// --- Witness management ---

func (s *SQLiteTraceStore) insertWitnessRecordIfMissing(plan witnessPlan, appendIntentID string) error {
	if s.factExists(plan.recordID) {
		existing, err := s.readFact(plan.recordID)
		if err != nil {
			return err
		}
		if !witnessFactMatchesPlan(existing, plan) {
			return &TraceStoreError{fmt.Sprintf("witness record id %q already names a different witness", plan.recordID)}
		}
		return nil
	}

	ordinal, err := s.nextOwnerOrdinal(witnessTraceOwnerID)
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

	if err := s.insertFactRow(fact, fmt.Sprintf("witness:%s", plan.recordID), appendIntentID); err != nil {
		return err
	}

	_, err = s.db.Exec(
		`INSERT INTO owner_ordinals(trace_owner_id, next_ordinal) VALUES (?, ?)
		 ON CONFLICT(trace_owner_id) DO UPDATE SET next_ordinal = excluded.next_ordinal`,
		witnessTraceOwnerID, ordinal+1,
	)
	return err
}

// --- Record insertion ---

func (s *SQLiteTraceStore) insertFactRow(fact Record, commitReceipt, appendIntentID string) error {
	if fact.View == nil {
		return &TraceStoreError{"path append requires record view metadata"}
	}
	if err := s.insertRecordIfMissing(fact, appendIntentID); err != nil {
		return err
	}

	_, err := s.db.Exec(`
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

func (s *SQLiteTraceStore) insertRecordIfMissing(fact Record, appendIntentID string) error {
	if fact.Envelope.RecordID != fact.Envelope.Digest {
		return &TraceStoreError{"record_id must equal digest"}
	}
	if s.factExists(fact.Envelope.RecordID) {
		existing, err := s.readRecord(fact.Envelope.RecordID)
		if err != nil {
			return err
		}
		if !recordContentMatches(existing, fact) {
			return &TraceStoreError{fmt.Sprintf("record id %q already names different content", fact.Envelope.RecordID)}
		}
		return nil
	}

	_, err := s.db.Exec(`
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
		_, err := s.db.Exec(
			"INSERT INTO record_edges(parent_record_id, child_record_id, parent_position) VALUES (?, ?, ?)",
			parent, fact.Envelope.RecordID, i,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SQLiteTraceStore) factExists(factID string) bool {
	var count int
	s.db.QueryRow("SELECT 1 FROM records WHERE record_id = ?", factID).Scan(&count)
	return count > 0
}

// --- Ordinal management ---

func (s *SQLiteTraceStore) nextOwnerOrdinal(ownerID string) (int, error) {
	var ordinal int
	err := s.db.QueryRow("SELECT next_ordinal FROM owner_ordinals WHERE trace_owner_id = ?", ownerID).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query owner ordinal: %w", err)
	}
	return ordinal, nil
}

func (s *SQLiteTraceStore) nextCommitSeq() (int, error) {
	var seq int
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = 'next_commit_seq'").Scan(&seq)
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
	r.Body = bodyFromJSON(bodyJSON)
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

	// Deduplicate
	seen := make(map[string]bool)
	var result []string
	for _, id := range group.CausalParents {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	for _, id := range draft.CausedByFactIDs {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	for _, id := range local {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, nil
}

func (s *SQLiteTraceStore) validateCausalParents(causedBy []string, stagedIDs map[string]bool) error {
	for _, id := range causedBy {
		if stagedIDs[id] {
			continue
		}
		if !s.factExists(id) {
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

func contextIDFor(appendIntentID string, groupIndex int, payload RetainedContext) string {
	data := map[string]any{
		"intent":      appendIntentID,
		"group_index": groupIndex,
		"payload": map[string]any{
			"active_binding_refs":       payload.ActiveBindingRefs,
			"capability_witness_refs":   payload.CapabilityWitnessRefs,
			"semantic_environment_refs": payload.SemanticEnvironmentRefs,
			"visibility_policy_refs":    payload.VisibilityPolicyRefs,
			"substrate_ref":            payload.SubstrateRef,
			"containment":              string(payload.Containment),
		},
	}
	b, _ := json.Marshal(data)
	return fmt.Sprintf("ctx:%x", sha256Sum(b))
}

func contextEqual(a, b RetainedContext) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}

func ordinaryWitnessPlan(ctx RetainedContext, opCtx OperationContext) witnessPlan {
	body := map[string]any{
		"actor_ref":                 opCtx.ActorRef,
		"authority_refs":            opCtx.PresentedAuthorityRefs,
		"active_binding_refs":       ctx.ActiveBindingRefs,
		"semantic_environment_refs": ctx.SemanticEnvironmentRefs,
		"visibility_policy_refs":    ctx.VisibilityPolicyRefs,
		"provenance_policy_refs":    []string{},
		"substrate_ref":            ctx.SubstrateRef,
		"containment":              string(ctx.Containment),
	}
	rootID := RootWitnessRecordIDMust()
	recordID, _ := RecordDigest(WitnessSchemaRef, Capture, body, nil, rootID)
	return witnessPlan{
		recordID:   recordID,
		schemaRef:  WitnessSchemaRef,
		kindLabel:  "witness",
		body:       body,
		witnessRef: rootID,
	}
}

func rootWitnessPlan() witnessPlan {
	body := RootWitnessBody()
	recordID := RootWitnessRecordIDMust()
	return witnessPlan{
		recordID:   recordID,
		schemaRef:  RootWitnessSchemaRef,
		kindLabel:  "root_witness",
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
		"schema_ref": a.Envelope.SchemaRef,
		"mode":       string(a.Envelope.Mode),
		"witness_ref": a.Envelope.WitnessRef,
		"caused_by":  a.Envelope.CausedByIDs,
		"body":       a.Body.Payload,
	})
	bJSON, _ := json.Marshal(map[string]any{
		"schema_ref": b.Envelope.SchemaRef,
		"mode":       string(b.Envelope.Mode),
		"witness_ref": b.Envelope.WitnessRef,
		"caused_by":  b.Envelope.CausedByIDs,
		"body":       b.Body.Payload,
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

func batchDigest(batch AppendBatch, ctx OperationContext) (string, error) {
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

func bodyFromJSON(s string) RecordBody {
	var payload map[string]any
	json.Unmarshal([]byte(s), &payload)
	return RecordBody{Payload: payload}
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

func mapToSlice[T any](m map[string]T) []T {
	s := make([]T, 0, len(m))
	for _, v := range m {
		s = append(s, v)
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
	contextID := contextIDFor(appendIntentID, groupIndex, payload)

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
		return Frontier{}, &TraceStoreError{fmt.Sprintf("unknown frontier id: %q", frontierID)}
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
