package shepherd

// vNext materialization orchestration over the Ring 0 trace store (plan 03
// §2), mirroring shepherd2/src/shepherd2/vnext/materialization.py.
//
// This is deliberately a vNext orchestrator over the store, not a frozen
// TraceStore method: Python keeps the same boundary (materialize() stays
// bound to the concrete SQLiteTraceStore because it reaches the
// SQLite-private completed-intent ledger).
//
// Ordering law (verified against Python, not guessed): captures are appended
// BEFORE the ledger entry is recorded. The ledger write is the commit point
// for idempotency; a crash between append and ledger leaves a replay that
// re-appends under the same intent id, and the store's own intent
// idempotency returns the original receipt, so at-least-once is safe.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// MaxOwnerOrdinal mirrors Python's MAX_OWNER_ORDINAL (2**63 - 1), the default
// cutoff for target reads.
const MaxOwnerOrdinal = math.MaxInt64

// materializeReadContext is the read context dispatch reads targets and
// witnesses through, mirroring Python's ReadContext(actor_ref="vnext:materialize").
var materializeReadContext = ReadContext{ActorRef: "vnext:materialize", VisibilityProfile: VisibilityPayload}

// MaterializationRequest is a path-explicit request to materialize retained
// declaration records. Bare record ids are insufficient — Python law: the
// same content appended under two owners allocates the same record id, so
// the owner path is what makes the request unambiguous.
type MaterializationRequest struct {
	// AppendIntentID is the idempotency key for the materialize transition.
	// The capture batch is appended under this same intent id, which is what
	// makes the crash window safe.
	AppendIntentID string
	// TargetTraceOwnerID is the owner path the targets must be present on.
	TargetTraceOwnerID string
	// TargetRecordIDs are the declaration records to materialize.
	TargetRecordIDs []string
	// TargetThroughOwnerOrdinal bounds the target read; targets beyond it
	// are invisible and therefore rejected. Callers wanting Python's default
	// pass MaxOwnerOrdinal; zero is a real cutoff at ordinal 0.
	TargetThroughOwnerOrdinal int
	// CaptureTraceOwnerID is the owner path captures are appended to; empty
	// means the target owner.
	CaptureTraceOwnerID string
}

// CaptureOwner returns the owner path materialization captures are appended to.
func (r MaterializationRequest) CaptureOwner() string {
	if r.CaptureTraceOwnerID != "" {
		return r.CaptureTraceOwnerID
	}
	return r.TargetTraceOwnerID
}

// Materialize materializes declarations through their witness-stamped
// substrate. It enforces the reserved OpMaterialize operation kind — the
// entry point that makes that vocabulary live — and replays stored receipts
// for repeated intents without re-dispatching the substrate.
func Materialize(ctx context.Context, store *SQLiteTraceStore, opCtx OperationContext, req MaterializationRequest, reg *SubstrateRegistry) (MaterializationReceipt, error) {
	if err := ensureMaterializeAuthorized(opCtx); err != nil {
		return MaterializationReceipt{}, err
	}
	if err := validateMaterializationRequest(req); err != nil {
		return MaterializationReceipt{}, err
	}
	digest, err := materializationRequestDigest(req, opCtx)
	if err != nil {
		return MaterializationReceipt{}, err
	}

	// Completed-intent ledger: same intent + same digest replays the stored
	// receipt without touching the substrate; same intent + different digest
	// is a conflict.
	if existingDigest, receipt, found, err := store.readCompletedMaterialization(req.AppendIntentID); err != nil {
		return MaterializationReceipt{}, err
	} else if found {
		if existingDigest != digest {
			return MaterializationReceipt{}, &AppendIntentConflictError{
				fmt.Sprintf("materialize intent %q was already committed with different content", req.AppendIntentID),
			}
		}
		return receipt, nil
	}

	targets, err := readMaterializationTargets(store, req)
	if err != nil {
		return MaterializationReceipt{}, err
	}

	substrateRef, err := substrateRefForRecords(store, targets)
	if err != nil {
		return MaterializationReceipt{}, err
	}
	substrate, err := reg.Get(substrateRef)
	if err != nil {
		return MaterializationReceipt{}, err
	}

	accepted := map[string]bool{}
	for _, s := range substrate.DeclarationSchemas() {
		accepted[s] = true
	}
	var unsupported []string
	for _, target := range targets {
		if !accepted[target.Envelope.SchemaRef] {
			unsupported = append(unsupported, target.Envelope.SchemaRef)
		}
	}
	if len(unsupported) > 0 {
		return MaterializationReceipt{}, &TraceStoreError{
			fmt.Sprintf("substrate %q does not accept schemas: %s", substrateRef, strings.Join(unsupported, ", ")),
		}
	}

	result, err := substrate.Materialize(ctx, targets)
	if err != nil {
		return MaterializationReceipt{}, err
	}
	switch result.Outcome {
	case MaterializationSuccess, MaterializationCleanFailure, MaterializationSplitState:
	default:
		return MaterializationReceipt{}, &TraceStoreError{fmt.Sprintf("unknown materialization outcome: %s", result.Outcome)}
	}
	captureSchemas := map[string]bool{}
	for _, s := range substrate.CaptureSchemas() {
		captureSchemas[s] = true
	}
	for _, draft := range result.CaptureDrafts {
		if draft.Mode != Capture {
			return MaterializationReceipt{}, &TraceStoreError{"materialize substrates may only emit capture drafts"}
		}
		if !captureSchemas[draft.SchemaRef] {
			return MaterializationReceipt{}, &TraceStoreError{
				fmt.Sprintf("substrate %q emitted unsupported capture schema %q", substrateRef, draft.SchemaRef),
			}
		}
	}

	var produced []string
	if len(result.CaptureDrafts) > 0 {
		appendReceipt, err := store.Append(
			AppendContext{
				ActorRef:             opCtx.ActorRef,
				PresentedWitnessRefs: append([]string{}, opCtx.PresentedAuthorityRefs...),
				SchemaVersionSet:     opCtx.SchemaEnvironmentRef,
				TrustMode:            opCtx.TrustMode,
			},
			AppendBatch{
				AppendIntentID: req.AppendIntentID,
				Groups: []AppendGroup{{
					TraceOwnerID: req.CaptureOwner(),
					RetainedContext: &RetainedContext{
						CapabilityWitnessRefs:   nonNilStrings(opCtx.PresentedAuthorityRefs),
						SemanticEnvironmentRefs: []string{opCtx.SchemaEnvironmentRef},
						SubstrateRef:            substrate.SubstrateRef(),
						Containment:             substrate.Containment(),
					},
					FactDrafts: result.CaptureDrafts,
				}},
			},
		)
		if err != nil {
			return MaterializationReceipt{}, err
		}
		produced = appendReceipt.FactIDs
	}

	receipt := MaterializationReceipt{
		Outcome:           result.Outcome,
		SubstrateRef:      substrateRef,
		TargetRecordIDs:   nonNilStrings(req.TargetRecordIDs),
		ProducedRecordIDs: nonNilStrings(produced),
		FailureReason:     result.FailureReason,
		// Normalized the same way the ledger serializes it, so a fresh
		// receipt and a replayed one are equal under reflect.DeepEqual —
		// Python gets this for free because its default is an empty tuple.
		WorldSideAnchors: nonNilAnchors(result.WorldSideAnchors),
	}
	if err := store.recordCompletedMaterialization(req.AppendIntentID, digest, receipt); err != nil {
		return MaterializationReceipt{}, err
	}
	return receipt, nil
}

// ensureMaterializeAuthorized mirrors _ensure_materialize_authorized: the
// operation must be materialize and the caller must carry the trusted
// internal witness. This is the check that makes OpMaterialize enforceable
// instead of reserved.
func ensureMaterializeAuthorized(ctx OperationContext) error {
	if ctx.Operation != OpMaterialize {
		return &TraceStoreError{"materialize operation requires OperationContext with Operation = materialize"}
	}
	if ctx.TrustMode == "internal" {
		return nil
	}
	for _, ref := range ctx.PresentedAuthorityRefs {
		if ref == "trusted:internal" {
			return nil
		}
	}
	return &TraceStoreError{"materialize requires trusted internal witness"}
}

// validateMaterializationRequest mirrors _validate_request.
func validateMaterializationRequest(req MaterializationRequest) error {
	if req.AppendIntentID == "" {
		return fmt.Errorf("append_intent_id is required")
	}
	if req.TargetTraceOwnerID == "" {
		return fmt.Errorf("target_trace_owner_id is required")
	}
	if len(req.TargetRecordIDs) == 0 {
		return fmt.Errorf("materialize requires at least one target record id")
	}
	if req.TargetThroughOwnerOrdinal < 0 {
		return fmt.Errorf("target_through_owner_ordinal must be non-negative")
	}
	return nil
}

// readMaterializationTargets resolves the request's record ids through the
// owner path: every target must be present on TargetTraceOwnerID at an
// ordinal <= the cutoff, and must be a declaration. A record id that exists
// but is not visible on this owner path is rejected — that is the point of
// the owner-path-explicit request shape.
func readMaterializationTargets(store *SQLiteTraceStore, req MaterializationRequest) ([]Record, error) {
	slice, err := store.ReadOwnerPrefix(materializeReadContext, req.TargetTraceOwnerID, req.TargetThroughOwnerOrdinal, ModeBoth)
	if err != nil {
		return nil, err
	}
	targets := make([]Record, 0, len(req.TargetRecordIDs))
	for _, recordID := range req.TargetRecordIDs {
		visible, ok := slice.FactsByID[recordID]
		if !ok {
			return nil, &TraceStoreError{
				fmt.Sprintf("materialize target %q is not present on owner path %q", recordID, req.TargetTraceOwnerID),
			}
		}
		record, isRecord := visible.(Record)
		if !isRecord {
			return nil, &TraceStoreError{
				fmt.Sprintf("materialize target %q is not payload-visible on owner path %q", recordID, req.TargetTraceOwnerID),
			}
		}
		if record.Envelope.Mode != Declaration {
			return nil, &TraceStoreError{"materialize targets must be declaration records"}
		}
		targets = append(targets, record)
	}
	return targets, nil
}

// substrateRefForRecords resolves each target's substrate through its
// witness record and requires the whole batch to name exactly one
// substrate. The witness is where the declaration's substrate was stamped at
// append time; materialize does not guess from schemas.
func substrateRefForRecords(store *SQLiteTraceStore, targets []Record) (string, error) {
	refs := []string{}
	seen := map[string]bool{}
	for _, record := range targets {
		visible, err := store.ReadFact(materializeReadContext, record.Envelope.WitnessRef)
		if err != nil {
			return "", &TraceStoreError{fmt.Sprintf("record witness %q is not payload-visible", record.Envelope.WitnessRef)}
		}
		witness, isRecord := visible.(Record)
		if !isRecord {
			return "", &TraceStoreError{fmt.Sprintf("record witness %q is not payload-visible", record.Envelope.WitnessRef)}
		}
		ref, _ := witness.Body.Payload["substrate_ref"].(string)
		if ref == "" {
			return "", &TraceStoreError{fmt.Sprintf("record witness %q has no substrate_ref", record.Envelope.WitnessRef)}
		}
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	if len(refs) != 1 {
		return "", &TraceStoreError{"materialize batch must target exactly one substrate"}
	}
	return refs[0], nil
}

// materializationRequestDigest mirrors _request_digest: sha256 over the
// ASCII-flavour JSON of the request plus the operation context. The flavour
// is the same one the store uses for context ids and stored receipts
// (CPython json.dumps defaults), NOT canonical v2 — the digest is an
// internal idempotency token, but it is pinned cross-language by the
// materialize vectors, so the encoder choice is load-bearing.
//
// Go's empty string maps to Python's None for the two Optional fields
// (capture_trace_owner_id, trust_mode): Python's dataclass distinguishes
// None from "", but "" never survives validation for capture_trace_owner_id,
// and a trust_mode of "" cannot be authorized, so the mapping is total.
func materializationRequestDigest(req MaterializationRequest, ctx OperationContext) (string, error) {
	payload := map[string]any{
		"append_intent_id":             req.AppendIntentID,
		"target_trace_owner_id":        req.TargetTraceOwnerID,
		"target_record_ids":            nonNilStrings(req.TargetRecordIDs),
		"target_through_owner_ordinal": req.TargetThroughOwnerOrdinal,
		"capture_trace_owner_id":       nullableJSONString(req.CaptureTraceOwnerID),
		"actor_ref":                    ctx.ActorRef,
		"authority_refs":               nonNilStrings(ctx.PresentedAuthorityRefs),
		"schema_environment_ref":       ctx.SchemaEnvironmentRef,
		"trust_mode":                   nullableJSONString(ctx.TrustMode),
	}
	body, err := canonicalJSONASCIIBytes(payload)
	if err != nil {
		return "", fmt.Errorf("materialization request digest: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// nullableJSONString renders "" as JSON null (Python None) and any other
// string as itself.
func nullableJSONString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// materializationReceiptJSON serializes a receipt for the ledger the way
// Python's _receipt_to_json does: sorted keys, compact separators, the
// ASCII flavour.
func materializationReceiptJSON(receipt MaterializationReceipt) (string, error) {
	body, err := canonicalJSONASCIIBytes(map[string]any{
		"outcome":             string(receipt.Outcome),
		"substrate_ref":       receipt.SubstrateRef,
		"target_record_ids":   nonNilStrings(receipt.TargetRecordIDs),
		"produced_record_ids": nonNilStrings(receipt.ProducedRecordIDs),
		"failure_reason":      receipt.FailureReason,
		"world_side_anchors":  nonNilAnchors(receipt.WorldSideAnchors),
	})
	if err != nil {
		return "", fmt.Errorf("materialization receipt encode: %w", err)
	}
	return string(body), nil
}

func materializationReceiptFromJSON(payload string) (MaterializationReceipt, error) {
	var stored struct {
		Outcome           string           `json:"outcome"`
		SubstrateRef      string           `json:"substrate_ref"`
		TargetRecordIDs   []string         `json:"target_record_ids"`
		ProducedRecordIDs []string         `json:"produced_record_ids"`
		FailureReason     string           `json:"failure_reason"`
		WorldSideAnchors  []map[string]any `json:"world_side_anchors"`
	}
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		return MaterializationReceipt{}, fmt.Errorf("materialization receipt decode: %w", err)
	}
	return MaterializationReceipt{
		Outcome:           MaterializationOutcome(stored.Outcome),
		SubstrateRef:      stored.SubstrateRef,
		TargetRecordIDs:   nonNilStrings(stored.TargetRecordIDs),
		ProducedRecordIDs: nonNilStrings(stored.ProducedRecordIDs),
		FailureReason:     stored.FailureReason,
		WorldSideAnchors:  nonNilAnchors(stored.WorldSideAnchors),
	}, nil
}

// nonNilAnchors returns an empty (non-nil) slice for nil input, so a receipt
// without anchors serializes as [] rather than null, matching Python's tuple.
func nonNilAnchors(anchors []map[string]any) []map[string]any {
	if anchors == nil {
		return []map[string]any{}
	}
	return anchors
}

// --- completed-intent ledger (store-private table) ---

// readCompletedMaterialization looks up the ledger for a materialize intent.
// The store lock is held only around the query, mirroring Python's
// _read_completed_intent; no lock is held across the substrate call or the
// capture append in Materialize.
func (s *SQLiteTraceStore) readCompletedMaterialization(intentID string) (string, MaterializationReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var digest, receiptJSON string
	err := s.db.QueryRow(
		"SELECT request_digest, receipt_json FROM materialization_intents WHERE materialize_intent_id = ?",
		intentID,
	).Scan(&digest, &receiptJSON)
	if err == sql.ErrNoRows {
		return "", MaterializationReceipt{}, false, nil
	}
	if err != nil {
		return "", MaterializationReceipt{}, false, fmt.Errorf("query materialization intent: %w", err)
	}
	receipt, err := materializationReceiptFromJSON(receiptJSON)
	if err != nil {
		return "", MaterializationReceipt{}, false, err
	}
	return digest, receipt, true, nil
}

// recordCompletedMaterialization writes the ledger entry — the idempotency
// commit point. The insert races only with a concurrent completion of the
// same intent; INSERT OR IGNORE plus a digest comparison decides replays the
// same way Python's IntegrityError branch does, without matching driver
// error strings.
func (s *SQLiteTraceStore) recordCompletedMaterialization(intentID, digest string, receipt MaterializationReceipt) error {
	receiptJSON, err := materializationReceiptJSON(receipt)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		"INSERT OR IGNORE INTO materialization_intents(materialize_intent_id, request_digest, receipt_json) VALUES (?, ?, ?)",
		intentID, digest, receiptJSON,
	)
	if err != nil {
		return fmt.Errorf("record materialization intent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}

	// The row exists: this intent was completed concurrently. Same digest
	// means the concurrent receipt describes the same transition — the
	// store-level intent idempotency guarantees identical produced ids —
	// so replay is a silent success. Different content is a conflict.
	var existingDigest string
	err = s.db.QueryRow(
		"SELECT request_digest FROM materialization_intents WHERE materialize_intent_id = ?",
		intentID,
	).Scan(&existingDigest)
	if err != nil {
		return fmt.Errorf("re-read materialization intent: %w", err)
	}
	if existingDigest != digest {
		return &AppendIntentConflictError{
			fmt.Sprintf("materialize intent %q was already committed with different content", intentID),
		}
	}
	return nil
}
