package shepherd

// Backend-agnostic TraceStore conformance suite (T1.7, plan 01 §5).
//
// Ported case-for-case from shepherd2/tests/test_trace_store_conformance.py.
// Subtests carry the Python test names (minus the test_ prefix) so the mapping
// is auditable without leaving this file; the full table, including the N/A
// rationale, lives in plans/01-abi-conformance.md §5.
//
// The suite is parametrized over a store factory so any future backend (plan
// 03 substrates, an in-memory test store) is gated by the same behavior, which
// is the durable "done" gate for a backend the way it is in Python.
//
// The harness binds each case to one directory for the case's lifetime and
// hands it to the factory on every open, so a case that closes and reopens
// reaches the same durable store — the property the restart case exists to
// check. The directory is created once per case by the harness rather than
// derived per call inside the factory: t.TempDir() returns a NEW directory on
// every call, and the first version of this harness did exactly that, letting
// the restart case pass against a fresh, empty store because content
// addressing makes the same append allocate the same ids anywhere.
//
// Deliberately out of scope, per PARITY-PLAN's resolution table: the eight
// run-output descriptor cases (descriptor projection/resolution roundtrip,
// visibility stability, output-name/frontier/owner mismatch, malformed
// citation, duplicate output names, store-wrapper resolution). They exercise
// shepherd2.schemas.run_outputs, which the Go port does not carry — that
// vocabulary lands with plan 04 (settlement) as a deliberately minimal subset,
// and the cases are re-evaluated there.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// ConformanceStore is the protocol surface this suite exercises. It is the Go
// counterpart of Python's TraceStore protocol, trimmed to the members the
// ported cases call; it grows as cases are added.
type ConformanceStore interface {
	Append(ctx context.Context, auth AppendContext, batch AppendBatch) (AppendReceipt, error)
	PreviewRecordIDs(ctx context.Context, auth AppendContext, batch AppendBatch) ([]string, error)
	ReadOwnerPrefix(ctx context.Context, auth ReadContext, ownerID string, through int, modeFilter ModeFilter) (Slice, error)
	ReadCausalClosure(ctx context.Context, auth ReadContext, roots []string, modeFilter ModeFilter, closurePolicy string) (Slice, error)
	PublishCut(ctx context.Context, auth AppendContext, spec FrontierSpec) (Frontier, error)
	ResolveCut(ctx context.Context, auth ReadContext, cutID string, modeFilter ModeFilter) (Slice, error)
	ResolveFrontier(ctx context.Context, auth ReadContext, frontierID string, modeFilter ModeFilter) (Slice, error)
	ReadOwnerCutoff(ctx context.Context, frontierID string) (Frontier, error)
	Close(ctx context.Context) error
}

// conformanceFactory opens (or reopens) the store at a fixed backing location
// under dir.
//
// dir is created once per case by the harness; every call with the same dir
// must reach the same durable store, which is the property the restart case
// exists to check. A backend that cannot be reopened at a stable location
// fails here, as in Python.
type conformanceFactory func(t *testing.T, dir string) ConformanceStore

// conformanceOpener is a conformanceFactory pre-bound to one case's directory.
// Cases see only this: every call reopens the same durable store.
type conformanceOpener func(t *testing.T) ConformanceStore

// conformanceAppend appends one group of drafts under one intent, mirroring
// the suite's _append helper in Python.
func conformanceAppend(t *testing.T, store ConformanceStore, intent, owner string, drafts ...RecordDraft) AppendReceipt {
	t.Helper()
	receipt, err := store.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: intent,
		Groups:         []AppendGroup{{TraceOwnerID: owner, FactDrafts: drafts}},
	})
	if err != nil {
		t.Fatalf("Append(%s): %v", intent, err)
	}
	return receipt
}

// assertFactIDsEqual reports a per-position diff of two fact-id lists.
func assertFactIDsEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d fact ids, want %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: fact[%d] = %q, want %q", label, i, got[i], want[i])
		}
	}
}

// runConformance runs every ported conformance case against one backend.
func runConformance(t *testing.T, open conformanceFactory) {
	cases := []struct {
		name string
		run  func(t *testing.T, open conformanceOpener)
	}{
		{"append_then_read_owner_prefix", conformanceAppendThenReadOwnerPrefix},
		{"append_intent_idempotent_across_restart", conformanceAppendIntentIdempotentAcrossRestart},
		{"same_intent_different_batch_is_rejected", conformanceSameIntentDifferentBatchIsRejected},
		{"preview_record_ids_match_append", conformancePreviewRecordIDsMatchAppend},
		{"fact_id_is_content_addressed_across_intents", conformanceFactIDIsContentAddressedAcrossIntents},
		{"content_addressed_fact_spans_multiple_owner_paths", conformanceContentAddressedFactSpansMultipleOwnerPaths},
		{"cut_publish_resolve_roundtrip", conformanceCutPublishResolveRoundtrip},
		{"read_owner_cutoff_roundtrips_a_published_cut", conformanceReadOwnerCutoffRoundtripsAPublishedCut},
		{"causal_closure_includes_parents", conformanceCausalClosureIncludesParents},
		{"causal_parent_must_exist", conformanceCausalParentMustExist},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One directory per case, created once. t.TempDir() returns a NEW
			// directory on every call, so the location cannot be derived inside
			// the factory: the restart case would reopen a fresh, empty store and
			// still pass, because content addressing makes the same append
			// allocate the same ids in any store. That is precisely the defect
			// review caught in the first version of this harness.
			dir := t.TempDir()
			tc.run(t, func(t *testing.T) ConformanceStore {
				return open(t, dir)
			})
		})
	}
}

// TestSQLiteTraceStoreConformance gates the SQLite reference backend.
func TestSQLiteTraceStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T, dir string) ConformanceStore {
		store, err := NewSQLiteTraceStore(filepath.Join(dir, "conformance.sqlite"))
		if err != nil {
			t.Fatalf("NewSQLiteTraceStore: %v", err)
		}
		return store
	})
}

// conformanceAppendThenReadOwnerPrefix ports test_append_then_read_owner_prefix.
func conformanceAppendThenReadOwnerPrefix(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	receipt := conformanceAppend(t, store, "intent:a", "exec:one",
		draft("step", Capture, map[string]any{"value": 1}),
	)
	slice, err := store.ReadOwnerPrefix(context.Background(), reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	assertFactIDsEqual(t, "ReadOwnerPrefix", slice.FactIDs(), receipt.FactIDs)
}

// conformanceAppendIntentIdempotentAcrossRestart ports
// test_append_intent_idempotent_across_restart.
//
// The reopened store is read *before* the retry append: the first session's
// facts must already be there, which proves the factory reopened the same
// durable store rather than a fresh one. This is the assertion that fails if
// that contract regresses — without it, a fresh-database bug would pass
// silently, because content addressing makes the same append allocate the
// same ids in any store.
//
// Python compares the whole receipt (second == first), and that is portable
// after all: an idempotent retry does not recompute anything, it returns the
// persisted receipt verbatim (receiptFromJSON of the stored receipt_json),
// so commit receipts, owner ranges, causal edges and context receipts must
// all match the first session's values — not merely the identity-bearing
// fields. A partial or recomputed retry receipt fails here.
func conformanceAppendIntentIdempotentAcrossRestart(t *testing.T, open conformanceOpener) {
	store := open(t)
	first := conformanceAppend(t, store, "intent:start", "exec:parent",
		draft("execution_started", Capture, map[string]any{"execution_id": "exec:parent"}),
	)
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}

	restarted := open(t)
	defer restarted.Close(context.Background())

	reopened, err := restarted.ReadOwnerPrefix(context.Background(), reader, "exec:parent", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix after reopen, before retry: %v", err)
	}
	assertFactIDsEqual(t, "read after reopen", reopened.FactIDs(), first.FactIDs)

	second := conformanceAppend(t, restarted, "intent:start", "exec:parent",
		draft("execution_started", Capture, map[string]any{"execution_id": "exec:parent"}),
	)
	assertAppendReceiptsEqual(t, "retried receipt", second, first)

	slice, err := restarted.ReadOwnerPrefix(context.Background(), reader, "exec:parent", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix after retry: %v", err)
	}
	assertFactIDsEqual(t, "read after retry", slice.FactIDs(), first.FactIDs)
}

// assertAppendReceiptsEqual compares two receipts field by field, including
// the seq-derived and map-shaped fields a whole-receipt equality needs. Nil
// and empty slices compare equal: the retry's receipt is the first's round
// trip through JSON, where both spellings persist as written.
func assertAppendReceiptsEqual(t *testing.T, label string, got, want AppendReceipt) {
	t.Helper()
	if got.AppendIntentID != want.AppendIntentID {
		t.Errorf("%s: append intent = %q, want %q", label, got.AppendIntentID, want.AppendIntentID)
	}
	assertStringSlicesEqual(t, label+" fact_ids", got.FactIDs, want.FactIDs)
	assertStringSlicesEqual(t, label+" commit_receipts", got.CommitReceipts, want.CommitReceipts)
	assertStringSlicesEqual(t, label+" context_receipts", got.ContextReceipts, want.ContextReceipts)

	if len(got.OwnerRanges) != len(want.OwnerRanges) {
		t.Errorf("%s: owner ranges = %v, want %v", label, got.OwnerRanges, want.OwnerRanges)
	} else {
		for owner, wantRange := range want.OwnerRanges {
			gotRange, ok := got.OwnerRanges[owner]
			if !ok {
				t.Errorf("%s: owner %s missing from %v", label, owner, got.OwnerRanges)
				continue
			}
			if gotRange != wantRange {
				t.Errorf("%s: owner %s range = %v, want %v", label, owner, gotRange, wantRange)
			}
		}
	}

	if len(got.CausalEdges) != len(want.CausalEdges) {
		t.Errorf("%s: causal edges = %v, want %v", label, got.CausalEdges, want.CausalEdges)
	} else {
		for i, edge := range want.CausalEdges {
			if got.CausalEdges[i] != edge {
				t.Errorf("%s: causal edge[%d] = %v, want %v", label, i, got.CausalEdges[i], edge)
			}
		}
	}
}

// assertStringSlicesEqual compares two string slices positionally, treating
// nil and empty as equal (see assertAppendReceiptsEqual for why).
func assertStringSlicesEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d entries, want %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: entry[%d] = %q, want %q", label, i, got[i], want[i])
		}
	}
}

// conformanceSameIntentDifferentBatchIsRejected ports
// test_same_intent_different_batch_is_rejected.
func conformanceSameIntentDifferentBatchIsRejected(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	conformanceAppend(t, store, "intent:once", "exec:one",
		draft("step", Capture, map[string]any{"value": 1}),
	)
	_, err := store.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: "intent:once",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:one",
			FactDrafts:   []RecordDraft{draft("step", Capture, map[string]any{"value": 2})},
		}},
	})
	var conflict *AppendIntentConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("reused intent: got %v, want AppendIntentConflictError", err)
	}

	slice, err := store.ReadOwnerPrefix(context.Background(), reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	ids := slice.FactIDs()
	if len(ids) != 1 {
		t.Fatalf("rejected batch left %d facts, want 1", len(ids))
	}
	fact, ok := slice.FactsByID[ids[0]].(Record)
	if !ok {
		t.Fatalf("expected a retained Record, got %T", slice.FactsByID[ids[0]])
	}
	// Python asserts payload == {"value": 1}; a retained body keeps JSON
	// integers as json.Number, so "1" is the same integer.
	value, ok := fact.Body.Payload["value"].(json.Number)
	if !ok {
		t.Fatalf("payload value is %T, want json.Number", fact.Body.Payload["value"])
	}
	if value.String() != "1" {
		t.Errorf("payload value = %q, want \"1\"", value.String())
	}
}

// conformancePreviewRecordIDsMatchAppend ports
// test_preview_record_ids_match_append.
//
// Python additionally asserts preview_fact_ids; the Go store exposes a single
// preview method, so that alias case is folded into this one.
func conformancePreviewRecordIDsMatchAppend(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	batch := AppendBatch{
		AppendIntentID: "intent:preview",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:preview",
			FactDrafts:   []RecordDraft{draft("step", Capture, map[string]any{"value": 1})},
		}},
	}
	previewed, err := store.PreviewRecordIDs(context.Background(), trustedAppend, batch)
	if err != nil {
		t.Fatalf("PreviewRecordIDs: %v", err)
	}
	receipt, err := store.Append(context.Background(), trustedAppend, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	assertFactIDsEqual(t, "preview", previewed, receipt.FactIDs)
}

// conformanceFactIDIsContentAddressedAcrossIntents ports
// test_fact_id_is_content_addressed_across_intents.
func conformanceFactIDIsContentAddressedAcrossIntents(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	d := draft("step", Capture, map[string]any{"value": 1})
	first := conformanceAppend(t, store, "intent:first", "exec:one", d)
	second := conformanceAppend(t, store, "intent:second", "exec:one", d)

	assertFactIDsEqual(t, "same content across intents", second.FactIDs, first.FactIDs)
	if len(first.FactIDs) == 0 || !strings.HasPrefix(first.FactIDs[0], "sha256:") {
		t.Errorf("fact id %v is not sha256-prefixed", first.FactIDs)
	}
	if first.CommitReceipts[0] == second.CommitReceipts[0] {
		t.Error("commit receipts should differ across intents")
	}
}

// conformanceContentAddressedFactSpansMultipleOwnerPaths ports
// test_content_addressed_fact_spans_multiple_owner_paths.
func conformanceContentAddressedFactSpansMultipleOwnerPaths(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	d := draft("step", Capture, map[string]any{"value": 1})
	first := conformanceAppend(t, store, "intent:owner-a", "owner:a", d)
	second := conformanceAppend(t, store, "intent:owner-b", "owner:b", d)
	assertFactIDsEqual(t, "same content across owners", second.FactIDs, first.FactIDs)

	sliceA, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:a", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(owner:a): %v", err)
	}
	sliceB, err := store.ReadOwnerPrefix(context.Background(), reader, "owner:b", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(owner:b): %v", err)
	}

	factA, ok := sliceA.FactsByID[first.FactIDs[0]].(Record)
	if !ok {
		t.Fatalf("owner:a: expected a retained Record, got %T", sliceA.FactsByID[first.FactIDs[0]])
	}
	factB, ok := sliceB.FactsByID[second.FactIDs[0]].(Record)
	if !ok {
		t.Fatalf("owner:b: expected a retained Record, got %T", sliceB.FactsByID[second.FactIDs[0]])
	}
	if factA.View.TraceOwnerID != "owner:a" {
		t.Errorf("owner:a fact trace_owner_id = %q, want \"owner:a\"", factA.View.TraceOwnerID)
	}
	if factB.View.TraceOwnerID != "owner:b" {
		t.Errorf("owner:b fact trace_owner_id = %q, want \"owner:b\"", factB.View.TraceOwnerID)
	}
}

// conformanceCutPublishResolveRoundtrip ports
// test_cut_publish_resolve_roundtrip.
func conformanceCutPublishResolveRoundtrip(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	receipt := conformanceAppend(t, store, "intent:cut", "exec:one",
		draft("a", Capture, nil),
		draft("b", Capture, nil),
	)
	cut, err := store.PublishCut(context.Background(), trustedAppend, FrontierSpec{
		FrontierID:         "frontier:cut",
		TargetTraceOwnerID: "exec:one",
		ThroughFactID:      receipt.FactIDs[len(receipt.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	slice, err := store.ResolveCut(context.Background(), reader, cut.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}
	assertFactIDsEqual(t, "ResolveCut", slice.FactIDs(), receipt.FactIDs)
}

// conformanceReadOwnerCutoffRoundtripsAPublishedCut ports
// test_read_owner_cutoff_roundtrips_a_published_cut.
func conformanceReadOwnerCutoffRoundtripsAPublishedCut(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	receipt := conformanceAppend(t, store, "intent:cutoff", "exec:one",
		draft("a", Capture, nil),
	)
	published, err := store.PublishCut(context.Background(), trustedAppend, FrontierSpec{
		FrontierID:         "frontier:cutoff",
		TargetTraceOwnerID: "exec:one",
		ThroughFactID:      receipt.FactIDs[len(receipt.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	cutoff, err := store.ReadOwnerCutoff(context.Background(), published.FrontierID)
	if err != nil {
		t.Fatalf("ReadOwnerCutoff: %v", err)
	}
	if cutoff.FrontierID != published.FrontierID {
		t.Errorf("cutoff frontier_id = %q, want %q", cutoff.FrontierID, published.FrontierID)
	}
	if cutoff.TargetTraceOwnerID != "exec:one" {
		t.Errorf("cutoff target_trace_owner_id = %q, want \"exec:one\"", cutoff.TargetTraceOwnerID)
	}

	slice, err := store.ResolveFrontier(context.Background(), reader, cutoff.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveFrontier: %v", err)
	}
	assertFactIDsEqual(t, "ResolveFrontier", slice.FactIDs(), receipt.FactIDs)
}

// conformanceCausalClosureIncludesParents ports
// test_causal_closure_includes_parents.
func conformanceCausalClosureIncludesParents(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	parent := conformanceAppend(t, store, "intent:parent", "exec:parent",
		draft("parent", Capture, nil),
	)
	parentID := parent.FactIDs[0]
	child := conformanceAppend(t, store, "intent:child", "exec:child",
		draft("child", Capture, nil, parentID),
	)
	childID := child.FactIDs[0]

	closure, err := store.ReadCausalClosure(context.Background(), reader, []string{childID}, ModeBoth, "include_external_anchors")
	if err != nil {
		t.Fatalf("ReadCausalClosure: %v", err)
	}
	ids := closure.FactIDs()
	foundChild, foundParent := false, false
	for _, id := range ids {
		switch id {
		case childID:
			foundChild = true
		case parentID:
			foundParent = true
		}
	}
	if !foundChild {
		t.Error("closure should include the child")
	}
	if !foundParent {
		t.Error("closure should include the parent")
	}
}

// conformanceCausalParentMustExist ports test_causal_parent_must_exist.
//
// Python expects the TraceStoreError base class; the Go error vocabulary has no
// shared base, and the concrete store error for a missing causal parent is
// UnknownFactError, so that is what the suite pins.
func conformanceCausalParentMustExist(t *testing.T, open conformanceOpener) {
	store := open(t)
	defer store.Close(context.Background())

	_, err := store.Append(context.Background(), trustedAppend, AppendBatch{
		AppendIntentID: "intent:orphan",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:one",
			FactDrafts:   []RecordDraft{draft("child", Capture, nil, "sha256:does-not-exist")},
		}},
	})
	var unknown *UnknownFactError
	if !errors.As(err, &unknown) {
		t.Fatalf("orphan causal parent: got %v, want UnknownFactError", err)
	}
}
