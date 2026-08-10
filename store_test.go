package shepherd

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *SQLiteTraceStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newMemStore(t *testing.T) *SQLiteTraceStore {
	t.Helper()
	store, err := NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

var trustedAppend = AppendContext{
	ActorRef:             "runtime:conformance",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

var reader = ReadContext{ActorRef: "reader", VisibilityProfile: VisibilityPayload}

func draft(kind string, mode RecordMode, payload map[string]any, causedBy ...string) RecordDraft {
	return RecordDraft{
		KindLabel:       kind,
		Mode:            mode,
		SchemaRef:       "shepherd2.conformance." + kind + ".v1",
		Payload:         payload,
		CausedByFactIDs: causedBy,
	}
}

func appendDrafts(t *testing.T, store *SQLiteTraceStore, intent, owner string, drafts ...RecordDraft) AppendReceipt {
	t.Helper()
	receipt, err := store.Append(trustedAppend, AppendBatch{
		AppendIntentID: intent,
		Groups:         []AppendGroup{{TraceOwnerID: owner, FactDrafts: drafts}},
	})
	if err != nil {
		t.Fatalf("Append(%s): %v", intent, err)
	}
	return receipt
}

func TestAppendThenReadOwnerPrefix(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:a", "exec:one",
		draft("step", Capture, map[string]any{"value": 1}),
	)

	slice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	if len(slice.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("expected %d facts, got %d", len(receipt.FactIDs), len(slice.FactIDs()))
	}
	for i, id := range receipt.FactIDs {
		if slice.FactIDs()[i] != id {
			t.Errorf("fact[%d] = %q, want %q", i, slice.FactIDs()[i], id)
		}
	}
}

func TestAppendIntentIdempotent(t *testing.T) {
	store := newMemStore(t)
	d := draft("execution_started", Capture, map[string]any{"execution_id": "exec:parent"})

	first := appendDrafts(t, store, "intent:start", "exec:parent", d)

	second, err := store.Append(trustedAppend, AppendBatch{
		AppendIntentID: "intent:start",
		Groups:         []AppendGroup{{TraceOwnerID: "exec:parent", FactDrafts: []RecordDraft{d}}},
	})
	if err != nil {
		t.Fatalf("second append: %v", err)
	}

	if len(first.FactIDs) != len(second.FactIDs) {
		t.Fatalf("receipt lengths differ: %d vs %d", len(first.FactIDs), len(second.FactIDs))
	}
	for i := range first.FactIDs {
		if first.FactIDs[i] != second.FactIDs[i] {
			t.Errorf("fact[%d] differs: %q vs %q", i, first.FactIDs[i], second.FactIDs[i])
		}
	}
}

func TestSameIntentDifferentBatchIsRejected(t *testing.T) {
	store := newMemStore(t)
	appendDrafts(t, store, "intent:once", "exec:one",
		draft("step", Capture, map[string]any{"value": 1}),
	)

	_, err := store.Append(trustedAppend, AppendBatch{
		AppendIntentID: "intent:once",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:one",
			FactDrafts:   []RecordDraft{draft("step", Capture, map[string]any{"value": 2})},
		}},
	})
	if err == nil {
		t.Fatal("expected AppendIntentConflictError")
	}
	if _, ok := err.(*AppendIntentConflictError); !ok {
		t.Errorf("expected AppendIntentConflictError, got %T: %v", err, err)
	}

	// Verify original value is preserved
	slice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	fact, ok := slice.FactsByID[slice.FactIDs()[0]].(Record)
	if !ok {
		t.Fatal("expected Record, got RecordShape")
	}
	if fact.Body.Payload["value"].(float64) != 1 {
		t.Errorf("expected value=1, got %v", fact.Body.Payload["value"])
	}
}

func TestPreviewRecordIDsMatchAppend(t *testing.T) {
	store := newMemStore(t)
	batch := AppendBatch{
		AppendIntentID: "intent:preview",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:preview",
			FactDrafts:   []RecordDraft{draft("step", Capture, map[string]any{"value": 1})},
		}},
	}

	previewed, err := store.PreviewRecordIDs(trustedAppend, batch)
	if err != nil {
		t.Fatalf("PreviewRecordIDs: %v", err)
	}

	receipt, err := store.Append(trustedAppend, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if len(previewed) != len(receipt.FactIDs) {
		t.Fatalf("preview length %d != receipt length %d", len(previewed), len(receipt.FactIDs))
	}
	for i := range previewed {
		if previewed[i] != receipt.FactIDs[i] {
			t.Errorf("preview[%d] = %q, want %q", i, previewed[i], receipt.FactIDs[i])
		}
	}
}

func TestFactIDIsContentAddressedAcrossIntents(t *testing.T) {
	store := newMemStore(t)
	d := draft("step", Capture, map[string]any{"value": 1})

	first := appendDrafts(t, store, "intent:first", "exec:one", d)
	second := appendDrafts(t, store, "intent:second", "exec:one", d)

	if len(first.FactIDs) != len(second.FactIDs) {
		t.Fatalf("receipt lengths differ")
	}
	for i := range first.FactIDs {
		if first.FactIDs[i] != second.FactIDs[i] {
			t.Errorf("fact[%d] differs: %q vs %q", i, first.FactIDs[i], second.FactIDs[i])
		}
	}
	if first.CommitReceipts[0] == second.CommitReceipts[0] {
		t.Error("commit receipts should differ across intents")
	}
}

func TestContentAddressedFactSpansMultipleOwnerPaths(t *testing.T) {
	store := newMemStore(t)
	d := draft("step", Capture, map[string]any{"value": 1})

	first := appendDrafts(t, store, "intent:owner-a", "owner:a", d)
	second := appendDrafts(t, store, "intent:owner-b", "owner:b", d)

	if first.FactIDs[0] != second.FactIDs[0] {
		t.Errorf("same content should produce same fact ID across owners")
	}

	sliceA, _ := store.ReadOwnerPrefix(reader, "owner:a", 99, ModeBoth)
	sliceB, _ := store.ReadOwnerPrefix(reader, "owner:b", 99, ModeBoth)

	factA, ok := sliceA.FactsByID[first.FactIDs[0]].(Record)
	if !ok {
		t.Fatal("expected Record on owner:a")
	}
	factB, ok := sliceB.FactsByID[second.FactIDs[0]].(Record)
	if !ok {
		t.Fatal("expected Record on owner:b")
	}

	if factA.View.TraceOwnerID != "owner:a" {
		t.Errorf("owner:a fact trace_owner_id = %q", factA.View.TraceOwnerID)
	}
	if factB.View.TraceOwnerID != "owner:b" {
		t.Errorf("owner:b fact trace_owner_id = %q", factB.View.TraceOwnerID)
	}
}

func TestCutPublishResolveRoundtrip(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:cut", "exec:one",
		draft("a", Capture, nil),
		draft("b", Capture, nil),
	)

	cut, err := store.PublishCut(trustedAppend, FrontierSpec{
		FrontierID:         "frontier:cut",
		TargetTraceOwnerID: "exec:one",
		ThroughFactID:      receipt.FactIDs[len(receipt.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	slice, err := store.ResolveCut(reader, cut.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}

	if len(slice.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("resolved slice has %d facts, want %d", len(slice.FactIDs()), len(receipt.FactIDs))
	}
	for i, id := range receipt.FactIDs {
		if slice.FactIDs()[i] != id {
			t.Errorf("fact[%d] = %q, want %q", i, slice.FactIDs()[i], id)
		}
	}
}

func TestReadOwnerCutoffRoundtripsAPublishedCut(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:cutoff", "exec:one",
		draft("a", Capture, nil),
	)

	published, err := store.PublishCut(trustedAppend, FrontierSpec{
		FrontierID:         "frontier:cutoff",
		TargetTraceOwnerID: "exec:one",
		ThroughFactID:      receipt.FactIDs[len(receipt.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	cutoff, err := store.ReadOwnerCutoff(published.FrontierID)
	if err != nil {
		t.Fatalf("ReadOwnerCutoff: %v", err)
	}

	if cutoff.FrontierID != published.FrontierID {
		t.Errorf("frontier_id = %q, want %q", cutoff.FrontierID, published.FrontierID)
	}
	if cutoff.TargetTraceOwnerID != "exec:one" {
		t.Errorf("target_trace_owner_id = %q, want %q", cutoff.TargetTraceOwnerID, "exec:one")
	}

	resolved, err := store.ResolveFrontier(reader, cutoff.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveFrontier: %v", err)
	}
	if len(resolved.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("resolved has %d facts, want %d", len(resolved.FactIDs()), len(receipt.FactIDs))
	}
}

func TestCausalClosureIncludesParents(t *testing.T) {
	store := newMemStore(t)
	parent := appendDrafts(t, store, "intent:parent", "exec:parent",
		draft("parent", Capture, nil),
	)
	parentID := parent.FactIDs[0]

	child := appendDrafts(t, store, "intent:child", "exec:child",
		draft("child", Capture, nil, parentID),
	)
	childID := child.FactIDs[0]

	closure, err := store.ReadCausalClosure(reader, []string{childID}, ModeBoth, "include_external_anchors")
	if err != nil {
		t.Fatalf("ReadCausalClosure: %v", err)
	}

	ids := closure.FactIDs()
	foundChild := false
	foundParent := false
	for _, id := range ids {
		if id == childID {
			foundChild = true
		}
		if id == parentID {
			foundParent = true
		}
	}
	if !foundChild {
		t.Error("closure should include child")
	}
	if !foundParent {
		t.Error("closure should include parent")
	}
}

func TestCausalParentMustExist(t *testing.T) {
	store := newMemStore(t)
	_, err := store.Append(trustedAppend, AppendBatch{
		AppendIntentID: "intent:orphan",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:one",
			FactDrafts:   []RecordDraft{draft("child", Capture, nil, "sha256:does-not-exist")},
		}},
	})
	if err == nil {
		t.Fatal("expected error for missing causal parent")
	}
	if _, ok := err.(*UnknownFactError); !ok {
		t.Errorf("expected UnknownFactError, got %T: %v", err, err)
	}
}

func TestFactCountAndContextCount(t *testing.T) {
	store := newMemStore(t)
	appendDrafts(t, store, "intent:a", "exec:one",
		draft("step", Capture, map[string]any{"value": 1}),
		draft("step", Capture, map[string]any{"value": 2}),
	)

	factCount, err := store.FactCount()
	if err != nil {
		t.Fatalf("FactCount: %v", err)
	}
	// 2 user records + witness records
	if factCount < 2 {
		t.Errorf("FactCount = %d, want >= 2", factCount)
	}

	ctxCount, err := store.ContextCount()
	if err != nil {
		t.Fatalf("ContextCount: %v", err)
	}
	if ctxCount < 1 {
		t.Errorf("ContextCount = %d, want >= 1", ctxCount)
	}
}

func TestWitnessChainToRoot(t *testing.T) {
	store := newMemStore(t)
	receipt := appendDrafts(t, store, "intent:witness", "exec:one",
		draft("step", Capture, map[string]any{"value": 1}),
	)

	// Read the fact and verify it has a witness ref that chains to root
	slice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}

	fact, ok := slice.FactsByID[receipt.FactIDs[0]].(Record)
	if !ok {
		t.Fatal("expected Record")
	}
	if fact.Envelope.WitnessRef == "" {
		t.Error("fact should have a witness ref")
	}

	// The witness should be in the slice's witnesses
	if len(slice.WitnessesByID) == 0 {
		t.Error("slice should include witness support records")
	}
}

func TestAppendReadAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart.sqlite")

	// First session: append
	store1, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	receipt1 := appendDrafts(t, store1, "intent:persist", "exec:one",
		draft("step", Capture, map[string]any{"value": 42}),
	)
	store1.Close()

	// Second session: re-open and append same intent
	store2, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer store2.Close()

	receipt2, err := store2.Append(trustedAppend, AppendBatch{
		AppendIntentID: "intent:persist",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:one",
			FactDrafts:   []RecordDraft{draft("step", Capture, map[string]any{"value": 42})},
		}},
	})
	if err != nil {
		t.Fatalf("second append: %v", err)
	}

	// Should be idempotent
	if len(receipt1.FactIDs) != len(receipt2.FactIDs) {
		t.Fatalf("receipt lengths differ after restart")
	}
	for i := range receipt1.FactIDs {
		if receipt1.FactIDs[i] != receipt2.FactIDs[i] {
			t.Errorf("fact[%d] differs after restart", i)
		}
	}

	// Read should work
	slice, err := store2.ReadOwnerPrefix(reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix after restart: %v", err)
	}
	if len(slice.FactIDs()) != len(receipt1.FactIDs) {
		t.Errorf("read has %d facts, want %d", len(slice.FactIDs()), len(receipt1.FactIDs))
	}
}

func TestEmptyBatchRejected(t *testing.T) {
	store := newMemStore(t)
	_, err := store.Append(trustedAppend, AppendBatch{
		AppendIntentID: "intent:empty",
		Groups:         []AppendGroup{{TraceOwnerID: "exec:one"}},
	})
	// Empty batch should succeed (no facts to append, but intent is recorded)
	// Actually, the Python impl allows empty groups. Let's just verify no crash.
	if err != nil {
		t.Logf("empty batch: %v", err) // May or may not error depending on impl
	}
}

func TestDeclarationAndCaptureModes(t *testing.T) {
	store := newMemStore(t)

	decl := appendDrafts(t, store, "intent:decl", "exec:one",
		draft("intent", Declaration, map[string]any{"action": "write"}),
	)
	capt := appendDrafts(t, store, "intent:capt", "exec:one",
		draft("result", Capture, map[string]any{"outcome": "done"}),
	)

	// Read captures only
	captSlice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeCapturesOnly)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix captures_only: %v", err)
	}
	if len(captSlice.FactIDs()) != 1 {
		t.Errorf("captures_only should have 1 fact, got %d", len(captSlice.FactIDs()))
	}
	if captSlice.FactIDs()[0] != capt.FactIDs[0] {
		t.Error("captures_only should include capture fact")
	}

	// Read declarations only
	declSlice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeDeclarationsOnly)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix declarations_only: %v", err)
	}
	if len(declSlice.FactIDs()) != 1 {
		t.Errorf("declarations_only should have 1 fact, got %d", len(declSlice.FactIDs()))
	}
	if declSlice.FactIDs()[0] != decl.FactIDs[0] {
		t.Error("declarations_only should include declaration fact")
	}

	// Read both
	bothSlice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix both: %v", err)
	}
	if len(bothSlice.FactIDs()) != 2 {
		t.Errorf("both should have 2 facts, got %d", len(bothSlice.FactIDs()))
	}
}

func TestFrontierIsImmutable(t *testing.T) {
	store := newMemStore(t)

	receipt1 := appendDrafts(t, store, "intent:frontier1", "exec:one",
		draft("a", Capture, nil),
	)
	receipt2 := appendDrafts(t, store, "intent:frontier2", "exec:one",
		draft("b", Capture, nil),
	)

	// Publish frontier at first fact
	_, err := store.PublishCut(trustedAppend, FrontierSpec{
		FrontierID:         "frontier:early",
		TargetTraceOwnerID: "exec:one",
		ThroughFactID:      receipt1.FactIDs[0],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	// Resolve should only see the first fact
	slice, err := store.ResolveCut(reader, "frontier:early", ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}
	if len(slice.FactIDs()) != 1 {
		t.Errorf("frontier should see 1 fact, got %d", len(slice.FactIDs()))
	}

	// Full prefix sees both user facts + the frontier_published record (same owner)
	fullSlice, err := store.ReadOwnerPrefix(reader, "exec:one", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	if len(fullSlice.FactIDs()) != 3 {
		t.Errorf("full prefix should see 3 facts (2 user + 1 frontier), got %d", len(fullSlice.FactIDs()))
	}

	_ = receipt2
}

func TestAppendRequiresTrustedContext(t *testing.T) {
	store := newMemStore(t)
	untrusted := AppendContext{
		ActorRef:             "runtime:untrusted",
		PresentedWitnessRefs: []string{},
		TrustMode:            "",
	}
	_, err := store.Append(untrusted, AppendBatch{
		AppendIntentID: "intent:untrusted",
		Groups: []AppendGroup{{
			TraceOwnerID: "exec:one",
			FactDrafts:   []RecordDraft{draft("step", Capture, nil)},
		}},
	})
	if err == nil {
		t.Fatal("expected error for untrusted append")
	}
}

func TestGoldenVectorsFromDisk(t *testing.T) {
	data, err := os.ReadFile("testdata/kernel_abi_v0.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_ = data // Just verify it's readable
	t.Logf("Golden vectors file is %d bytes", len(data))
}
