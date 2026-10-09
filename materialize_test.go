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

// anchorSubstrate emits a numeric world-side anchor, the shape a substrate
// reading declarations from the trace produces (json.Number). The ledger
// replay must return a receipt equal to the fresh one; the receipt decode
// uses UseNumber for exactly this.
type anchorSubstrate struct {
	calls int
}

func (a *anchorSubstrate) SubstrateRef() string         { return "test.anchor.v1" }
func (a *anchorSubstrate) DeclarationSchemas() []string { return []string{"example.write.v1"} }
func (a *anchorSubstrate) CaptureSchemas() []string     { return []string{"example.write.applied.v1"} }
func (a *anchorSubstrate) Containment() Containment     { return ContainContained }

func (a *anchorSubstrate) Materialize(_ context.Context, records []Record) (MaterializationResult, error) {
	a.calls++
	return MaterializationResult{
		Outcome: MaterializationSuccess,
		CaptureDrafts: []RecordDraft{{
			Mode:            Capture,
			SchemaRef:       "example.write.applied.v1",
			KindLabel:       "write_applied",
			Payload:         map[string]any{"call": a.calls},
			CausedByFactIDs: []string{records[0].Envelope.RecordID},
		}},
		WorldSideAnchors: []map[string]any{{
			"kind":          "counter",
			"count":         json.Number("41"),
			"substrate_ref": "test.anchor.v1",
		}},
	}, nil
}

// TestMaterializeReceiptReplayPreservesAnchorValueTypes pins that a replayed
// receipt decodes its numeric anchors back to the same value a fresh receipt
// carries. Without UseNumber decoding the replayed count would be float64
// and the receipts would diverge under reflect.DeepEqual.
func TestMaterializeReceiptReplayPreservesAnchorValueTypes(t *testing.T) {
	store := newMemStore(t)
	substrate := &anchorSubstrate{}
	registry := NewSubstrateRegistry()
	if err := registry.Register(substrate); err != nil {
		t.Fatalf("register: %v", err)
	}
	declaration, err := store.Append(materializeAppend, declareBatch(
		"intent:anchor-declaration", "owner:anchor", "test.anchor.v1",
		RecordDraft{Mode: Declaration, SchemaRef: "example.write.v1", Payload: map[string]any{"value": jsonNumber(t, 1)}},
	))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	req := MaterializationRequest{
		AppendIntentID:            "intent:anchor-apply",
		TargetTraceOwnerID:        "owner:anchor",
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

	if !reflect.DeepEqual(first, second) {
		t.Errorf("replayed receipt diverged:\nfirst  = %#v\nsecond = %#v", first, second)
	}
	if got, ok := second.WorldSideAnchors[0]["count"].(json.Number); !ok || got != json.Number("41") {
		t.Errorf("replayed anchor count = %v (%T), want json.Number 41", second.WorldSideAnchors[0]["count"], second.WorldSideAnchors[0]["count"])
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
		RecordDraft{Mode: Declaration, SchemaRef: 