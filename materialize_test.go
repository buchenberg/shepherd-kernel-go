package shepherd

// Materialize dispatch tests (plan 03 §4), ported case-for-case from
// shepherd2/tests/test_materialize.py. Subtests carry the Python test names
// (minus the test_ prefix) so the mapping is auditable without leaving this
// file, matching the conformance suite convention. Go-only cases (the crash
// window, the operation-kind gate, the capture-owner override) carry
// names of their own.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

var materializeAppend = AppendContext{
	ActorRef:             "runtime:test",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

// materializeContext mirrors the Python suite's MATERIALIZE context.
func materializeContext() OperationContext {
	return materializeAppend.ToOperationContext(OpMaterialize)
}

func declareBatch(intent, owner, substrateRef string, drafts ...RecordDraft) AppendBatch {
	return AppendBatch{
		AppendIntentID: intent,
		Groups: []AppendGroup{{
			TraceOwnerID: owner,
			RetainedContext: &RetainedContext{
				SubstrateRef: substrateRef,
				Containment:  ContainContained,
			},
			FactDrafts: drafts,
		}},
	}
}

func TestMaterializeDispatchesViaWitnessStampedSubstrate(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:declare-write", "owner:materialize", "test.echo.v1",
		RecordDraft{
			Mode:      Declaration,
			SchemaRef: "example.write.v1",
			KindLabel: "write",
			Payload:   map[string]any{"path": "note.txt", "text": "hello"},
		},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	receipt, err := Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:materialize-write",
		TargetTraceOwnerID:        "owner:materialize",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if receipt.Outcome != MaterializationSuccess {
		t.Errorf("outcome = %q, want success", receipt.Outcome)
	}
	if receipt.SubstrateRef != "test.echo.v1" {
		t.Errorf("substrate_ref = %q, want test.echo.v1", receipt.SubstrateRef)
	}
	if !reflect.DeepEqual(receipt.TargetRecordIDs, declaration.FactIDs) {
		t.Errorf("target ids = %v, want %v", receipt.TargetRecordIDs, declaration.FactIDs)
	}
	if len(receipt.ProducedRecordIDs) != 1 {
		t.Fatalf("produced ids = %v, want one", receipt.ProducedRecordIDs)
	}

	capture, err := store.ReadFact(reader, receipt.ProducedRecordIDs[0])
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	rec, ok := capture.(Record)
	if !ok {
		t.Fatalf("capture is a shape, not a record: %T", capture)
	}
	if rec.Envelope.Mode != Capture {
		t.Errorf("capture mode = %q, want capture", rec.Envelope.Mode)
	}
	if rec.Envelope.SchemaRef != "example.write.v1" {
		t.Errorf("capture schema = %q, want example.write.v1", rec.Envelope.SchemaRef)
	}
	if !reflect.DeepEqual(rec.Envelope.CausedByIDs, declaration.FactIDs) {
		t.Errorf("capture caused_by = %v, want %v", rec.Envelope.CausedByIDs, declaration.FactIDs)
	}
	if !reflect.DeepEqual(rec.Body.Payload, map[string]any{"path": "note.txt", "text": "hello"}) {
		t.Errorf("capture payload = %v", rec.Body.Payload)
	}

	witness, err := store.ReadFact(reader, rec.Envelope.WitnessRef)
	if err != nil {
		t.Fatalf("read witness: %v", err)
	}
	w, ok := witness.(Record)
	if !ok {
		t.Fatalf("witness is a shape, not a record: %T", witness)
	}
	if w.Body.Payload["substrate_ref"] != "test.echo.v1" {
		t.Errorf("witness substrate_ref = %v, want test.echo.v1", w.Body.Payload["substrate_ref"])
	}
	if w.Body.Payload["containment"] != "contained" {
		t.Errorf("witness containment = %v, want contained", w.Body.Payload["containment"])
	}
}

func TestMaterializeRejectsCaptureTargets(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	capture, err := store.Append(materializeAppend, declareBatch(
		"intent:capture-only", "owner:materialize", "test.echo.v1",
		RecordDraft{Mode: Capture, SchemaRef: "example.write.v1", KindLabel: "write", Payload: map[string]any{"path": "note.txt"}},
	))
	if err != nil {
		t.Fatalf("append capture: %v", err)
	}

	_, err = Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:bad-materialize",
		TargetTraceOwnerID:        "owner:materialize",
		TargetRecordIDs:           capture.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err == nil {
		t.Fatal("materialize of a capture target succeeded, want declaration error")
	}
	if !strings.Contains(err.Error(), "declaration") {
		t.Errorf("error = %q, want it to mention declaration records", err.Error())
	}
}

func TestMaterializeFailsClosedForUnregisteredSubstrate(t *testing.T) {
	store := newMemStore(t)
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:unknown-substrate", "owner:materialize", "missing.substrate.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", KindLabel: "write", Payload: map[string]any{"path": "note.txt"}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	_, err = Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:missing-substrate",
		TargetTraceOwnerID:        "owner:materialize",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, NewSubstrateRegistry())
	if !errors.Is(err, ErrUnknownSubstrate) {
		t.Fatalf("error = %v, want ErrUnknownSubstrate", err)
	}
	if !strings.Contains(err.Error(), "missing.substrate.v1") {
		t.Errorf("error = %q, want it to name the substrate", err.Error())
	}
}

func TestMaterializeWithDeterministicSQLiteKVSubstrate(t *testing.T) {
	store := newMemStore(t)
	kv, err := NewKVSubstrate(filepath.Join(t.TempDir(), "world.sqlite"))
	if err != nil {
		t.Fatalf("NewKVSubstrate: %v", err)
	}
	defer kv.Close()

	registry := NewSubstrateRegistry()
	if err := registry.Register(kv); err != nil {
		t.Fatalf("register: %v", err)
	}
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:kv-put", "owner:kv", KVSQLiteSubstrateRef,
		RecordDraft{
			Mode:      Declaration,
			SchemaRef: KVPutDeclarationSchema,
			KindLabel: "kv_put",
			Payload:   map[string]any{"key": "answer", "value": map[string]any{"n": jsonNumber(t, 42)}},
		},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	receipt, err := Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:kv-apply",
		TargetTraceOwnerID:        "owner:kv",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if got, err := kv.Get("answer"); err != nil || !reflect.DeepEqual(got, map[string]any{"n": jsonNumber(t, 42)}) {
		t.Errorf("kv[answer] = %v, %v; want {n: 42}", got, err)
	}

	if receipt.Outcome != MaterializationSuccess {
		t.Errorf("outcome = %q, want success", receipt.Outcome)
	}
	if receipt.SubstrateRef != KVSQLiteSubstrateRef {
		t.Errorf("substrate_ref = %q, want %q", receipt.SubstrateRef, KVSQLiteSubstrateRef)
	}
	wantAnchors := []map[string]any{{
		"kind":          "kv_key",
		"key":           "answer",
		"substrate_ref": KVSQLiteSubstrateRef,
	}}
	if !reflect.DeepEqual(receipt.WorldSideAnchors, wantAnchors) {
		t.Errorf("anchors = %v, want %v", receipt.WorldSideAnchors, wantAnchors)
	}

	capture, err := store.ReadFact(reader, receipt.ProducedRecordIDs[0])
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	rec := capture.(Record)
	if rec.Envelope.Mode != Capture || rec.Envelope.SchemaRef != KVPutCaptureSchema {
		t.Errorf("capture = %q/%q, want capture/%q", rec.Envelope.Mode, rec.Envelope.SchemaRef, KVPutCaptureSchema)
	}
	if !reflect.DeepEqual(rec.Envelope.CausedByIDs, declaration.FactIDs) {
		t.Errorf("capture caused_by = %v, want %v", rec.Envelope.CausedByIDs, declaration.FactIDs)
	}
	if !reflect.DeepEqual(rec.Body.Payload, map[string]any{"key": "answer", "value": map[string]any{"n": jsonNumber(t, 42)}}) {
		t.Errorf("capture payload = %v", rec.Body.Payload)
	}
	witness, err := store.ReadFact(reader, rec.Envelope.WitnessRef)
	if err != nil {
		t.Fatalf("read witness: %v", err)
	}
	if w := witness.(Record); w.Body.Payload["substrate_ref"] != KVSQLiteSubstrateRef {
		t.Errorf("witness substrate_ref = %v, want %q", w.Body.Payload["substrate_ref"], KVSQLiteSubstrateRef)
	}
}

// countingSubstrate mirrors the Python suite's CountingSubstrate: it counts
// calls and emits a payload naming the call, so a redispatch is visible in
// the capture content too, not just the counter.
type countingSubstrate struct {
	calls int
}

func (c *countingSubstrate) SubstrateRef() string         { return "test.count.v1" }
func (c *countingSubstrate) DeclarationSchemas() []string { return []string{"example.write.v1"} }
func (c *countingSubstrate) CaptureSchemas() []string     { return []string{"example.write.applied.v1"} }
func (c *countingSubstrate) Containment() Containment     { return ContainContained }

func (c *countingSubstrate) Materialize(_ context.Context, records []Record) (MaterializationResult, error) {
	c.calls++
	return MaterializationResult{
		Outcome: MaterializationSuccess,
		CaptureDrafts: []RecordDraft{{
			Mode:            Capture,
			SchemaRef:       "example.write.applied.v1",
			KindLabel:       "write_applied",
			Payload:         map[string]any{"call": c.calls},
			CausedByFactIDs: []string{records[0].Envelope.RecordID},
		}},
	}, nil
}

func TestMaterializeRetryReturnsReceiptWithoutRedispatchingSubstrate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "trace.sqlite")
	substrate := &countingSubstrate{}
	registry := NewSubstrateRegistry()
	if err := registry.Register(substrate); err != nil {
		t.Fatalf("register: %v", err)
	}

	store, err := NewSQLiteTraceStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:count-declaration", "owner:count", "test.count.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", KindLabel: "write", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	req := MaterializationRequest{
		AppendIntentID:            "intent:count-apply",
		TargetTraceOwnerID:        "owner:count",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}

	first, err := Materialize(context.Background(), store, materializeContext(), req, registry)
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	second, err := Materialize(context.Background(), store, materializeContext(), req, registry)
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	store.Close()

	// Restart: the ledger is durable, so a restarted process replays the
	// stored receipt without redispatching the substrate.
	restarted, err := NewSQLiteTraceStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer restarted.Close()
	third, err := Materialize(context.Background(), restarted, materializeContext(), req, registry)
	if err != nil {
		t.Fatalf("third materialize: %v", err)
	}

	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(second, third) {
		t.Errorf("receipts diverged:\nfirst  = %+v\nsecond = %+v\nthird  = %+v", first, second, third)
	}
	if substrate.calls != 1 {
		t.Errorf("substrate calls = %d, want 1", substrate.calls)
	}
}

func TestMaterializeIntentConflictIsRejectedBeforeSubstrateDispatch(t *testing.T) {
	store := newMemStore(t)
	substrate := &countingSubstrate{}
	registry := NewSubstrateRegistry()
	if err := registry.Register(substrate); err != nil {
		t.Fatalf("register: %v", err)
	}

	first, err := store.Append(materializeAppend, declareBatch(
		"intent:count-conflict:first", "owner:count-conflict", "test.count.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	second, err := store.Append(materializeAppend, declareBatch(
		"intent:count-conflict:second", "owner:count-conflict", "test.count.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 2)}},
	))
	if err != nil {
		t.Fatalf("append second: %v", err)
	}

	if _, err := Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:count-conflict:apply",
		TargetTraceOwnerID:        "owner:count-conflict",
		TargetRecordIDs:           first.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry); err != nil {
		t.Fatalf("first materialize: %v", err)
	}

	_, err = Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:count-conflict:apply",
		TargetTraceOwnerID:        "owner:count-conflict",
		TargetRecordIDs:           second.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err == nil {
		t.Fatal("conflicting materialize succeeded, want conflict error")
	}
	if _, ok := err.(*AppendIntentConflictError); !ok {
		t.Errorf("error = %T (%v), want *AppendIntentConflictError", err, err)
	}
	if !strings.Contains(err.Error(), "different content") {
		t.Errorf("error = %q, want it to say 'different content'", err.Error())
	}
	if substrate.calls != 1 {
		t.Errorf("substrate calls = %d, want 1 — the conflict must be rejected before dispatch", substrate.calls)
	}
}

func TestMaterializeTargetsExplicitOwnerPathForSharedRecords(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The same declaration content appended under two owners allocates the
	// same record id — content addressing. The owner path is therefore the
	// only thing that disambiguates a materialization request.
	draft := RecordDraft{
		Mode:      Declaration,
		SchemaRef: "example.write.v1",
		KindLabel: "write",
		Payload:   map[string]any{"path": "note.txt", "text": "hello"},
	}
	first, err := store.Append(materializeAppend, declareBatch("intent:shared:first", "owner:z", "test.echo.v1", draft))
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	second, err := store.Append(materializeAppend, declareBatch("intent:shared:second", "owner:a", "test.echo.v1", draft))
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if !reflect.DeepEqual(first.FactIDs, second.FactIDs) {
		t.Fatalf("shared content produced different ids: %v vs %v", first.FactIDs, second.FactIDs)
	}

	receipt, err := Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:shared:apply",
		TargetTraceOwnerID:        "owner:z",
		TargetRecordIDs:           first.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	zSlice, err := store.ReadOwnerPrefix(reader, "owner:z", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read owner:z: %v", err)
	}
	aSlice, err := store.ReadOwnerPrefix(reader, "owner:a", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read owner:a: %v", err)
	}
	if _, ok := zSlice.FactsByID[receipt.ProducedRecordIDs[0]]; !ok {
		t.Error("produced capture missing from owner:z")
	}
	if _, ok := aSlice.FactsByID[receipt.ProducedRecordIDs[0]]; ok {
		t.Error("produced capture leaked onto owner:a")
	}
}

func TestMaterializeRejectsRecordMissingFromTargetOwnerPath(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:wrong-owner", "owner:right", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	_, err = Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:wrong-owner:apply",
		TargetTraceOwnerID:        "owner:wrong",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err == nil {
		t.Fatal("materialize against the wrong owner succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not present on owner path") {
		t.Errorf("error = %q, want 'not present on owner path'", err.Error())
	}
}

// --- Go-only cases ---

// TestMaterializeRejectsNonMaterializeOperationContext pins the operation
// gate that makes OpMaterialize enforceable — the inversion
// TestReservedOperationKindsAreUnreachable anticipated (plan 03 §2).
func TestMaterializeRejectsNonMaterializeOperationContext(t *testing.T) {
	store := newMemStore(t)
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:op-gate", "owner:gate", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := MaterializationRequest{
		AppendIntentID:            "intent:op-gate:apply",
		TargetTraceOwnerID:        "owner:gate",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}

	// An append-context operation kind is not a materialize context.
	if _, err := Materialize(context.Background(), store, materializeAppend.ToOperationContext(OpAppend), req, registry); err == nil {
		t.Error("materialize accepted an append operation context")
	} else if !strings.Contains(err.Error(), "materialize") {
		t.Errorf("error = %q, want it to name the materialize operation requirement", err.Error())
	}

	// And an untrusted materialize context is rejected too.
	untrusted := OperationContext{
		ActorRef:               "runtime:test",
		Operation:              OpMaterialize,
		PresentedAuthorityRefs: []string{"untrusted:other"},
		SchemaEnvironmentRef:   "shepherd2-slice-a",
	}
	if _, err := Materialize(context.Background(), store, untrusted, req, registry); err == nil {
		t.Error("materialize accepted an untrusted context")
	} else if !strings.Contains(err.Error(), "trusted internal witness") {
		t.Errorf("error = %q, want trusted-internal-witness rejection", err.Error())
	}
}

// TestMaterializeCrashWindowReplayIsConsistent simulates the crash between
// the capture append and the ledger write (plan 03 §2 ordering): the ledger
// row is removed after a completed materialize, exactly the state a crash
// would leave. The replay must be consistent: the substrate is redispatched
// (the ledger is empty, so dispatch cannot know the intent completed), and
// for a deterministic substrate the store's own intent idempotency returns
// the original append receipt — no new records, and the ledger is repaired.
//
// A NON-deterministic substrate fails this window loudly instead: its second
// dispatch emits different drafts, the store rejects the same intent with
// different content, and the caller learns. That is the honest failure; the
// crash window makes substrate determinism load-bearing, which is why the
// reference substrates (Echo, KV) are deterministic.
func TestMaterializeCrashWindowReplayIsConsistent(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:crash-declaration", "owner:crash", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", KindLabel: "write", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	req := MaterializationRequest{
		AppendIntentID:            "intent:crash-apply",
		TargetTraceOwnerID:        "owner:crash",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}

	first, err := Materialize(context.Background(), store, materializeContext(), req, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	factsBefore, err := store.FactCount()
	if err != nil {
		t.Fatalf("fact count: %v", err)
	}

	// Simulate the crash window: the captures are committed, the ledger
	// entry is not.
	if _, err := store.db.Exec("DELETE FROM materialization_intents WHERE materialize_intent_id = ?", req.AppendIntentID); err != nil {
		t.Fatalf("delete ledger row: %v", err)
	}

	replay, err := Materialize(context.Background(), store, materializeContext(), req, registry)
	if err != nil {
		t.Fatalf("replay after crash window: %v", err)
	}
	factsAfter, err := store.FactCount()
	if err != nil {
		t.Fatalf("fact count: %v", err)
	}

	if !reflect.DeepEqual(first, replay) {
		t.Errorf("replay receipt diverged:\nfirst  = %+v\nreplay = %+v", first, replay)
	}
	if factsBefore != factsAfter {
		t.Errorf("fact count before replay = %d, after = %d — replay must not append new records", factsBefore, factsAfter)
	}

	// The ledger is repaired: a third call now replays without dispatch.
	repaired, err := Materialize(context.Background(), store, materializeContext(), req, registry)
	if err != nil {
		t.Fatalf("materialize after repair: %v", err)
	}
	if !reflect.DeepEqual(first, repaired) {
		t.Errorf("post-repair receipt diverged:\nfirst   = %+v\nrepaired = %+v", first, repaired)
	}
}

// TestMaterializeCaptureOwnerOverride pins CaptureTraceOwnerID: captures may
// be appended to a different owner path than the declarations live on.
func TestMaterializeCaptureOwnerOverride(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:owner-override", "owner:decl", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	receipt, err := Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:owner-override:apply",
		TargetTraceOwnerID:        "owner:decl",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
		CaptureTraceOwnerID:       "owner:captures",
	}, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	slice, err := store.ReadOwnerPrefix(reader, "owner:captures", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read capture owner: %v", err)
	}
	if _, ok := slice.FactsByID[receipt.ProducedRecordIDs[0]]; !ok {
		t.Error("capture missing from the explicit capture owner path")
	}
}

// TestMaterializeRejectsTargetBeyondOrdinalCutoff pins the ordinal gate:
// a target present on the owner path but beyond the request's cutoff is
// invisible and therefore rejected.
func TestMaterializeRejectsTargetBeyondOrdinalCutoff(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:cutoff", "owner:cutoff", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	// The declaration sits at ordinal 0; a cutoff of -1 is impossible, so
	// use a store with a second fact and a cutoff of 0 targeting it.
	_, err = store.Append(materializeAppend, declareBatch(
		"intent:cutoff:second", "owner:cutoff", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 2)}},
	))
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	secondSlice, err := store.ReadOwnerPrefix(reader, "owner:cutoff", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read owner: %v", err)
	}
	var secondID string
	for _, id := range secondSlice.FactIDs() {
		if id != declaration.FactIDs[0] {
			secondID = id
		}
	}
	if secondID == "" {
		t.Fatal("no second fact found")
	}

	_, err = Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:cutoff:apply",
		TargetTraceOwnerID:        "owner:cutoff",
		TargetRecordIDs:           []string{secondID},
		TargetThroughOwnerOrdinal: 0, // cutoff excludes the ordinal-1 fact
	}, registry)
	if err == nil {
		t.Fatal("materialize beyond the ordinal cutoff succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not present on owner path") {
		t.Errorf("error = %q, want 'not present on owner path'", err.Error())
	}
}

// TestMaterializeRejectsMixedSubstrateBatch pins the single-substrate law:
// one materialize transition dispatches to exactly one substrate.
func TestMaterializeRejectsMixedSubstrateBatch(t *testing.T) {
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate("test.echo.v1", "example.write.v1")); err != nil {
		t.Fatalf("register: %v", err)
	}

	first, err := store.Append(materializeAppend, declareBatch(
		"intent:mixed:one", "owner:mixed", "test.echo.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	second, err := store.Append(materializeAppend, declareBatch(
		"intent:mixed:two", "owner:mixed", "kv.sqlite.local.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 2)}},
	))
	if err != nil {
		t.Fatalf("append second: %v", err)
	}

	_, err = Materialize(context.Background(), store, materializeContext(), MaterializationRequest{
		AppendIntentID:            "intent:mixed:apply",
		TargetTraceOwnerID:        "owner:mixed",
		TargetRecordIDs:           append(append([]string{}, first.FactIDs...), second.FactIDs...),
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}, registry)
	if err == nil {
		t.Fatal("mixed-substrate materialize succeeded, want error")
	}
	if !strings.Contains(err.Error(), "exactly one substrate") {
		t.Errorf("error = %q, want 'exactly one substrate'", err.Error())
	}
}

func jsonNumber(t *testing.T, n int) json.Number {
	t.Helper()
	return json.Number(strconv.Itoa(n))
}
