package shepherd

// Cross-language materialize vectors (T2b.5, plan 03 §4): replay the
// sequences recorded through the Python reference and require identical
// identities — declaration fact ids, produced capture record ids, receipts,
// request digests — and identical retained content.
//
// Regenerate with:
//
//	python testdata/generate_materialize_vectors.py testdata/materialize_vectors_v0.json
//
// (updating the pinned SHA-256 in golden_provenance_test.go in the same
// commit). The file's hash is pinned there; this test never writes it.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type materializeVectorDoc struct {
	Generator      string                    `json:"generator"`
	SourceRepo     string                    `json:"source_repo"`
	SourceCommit   string                    `json:"source_commit"`
	PythonVersion  string                    `json:"python_version"`
	RequestDigests []materializeDigestVector `json:"request_digests"`
	EchoSequence   materializeSequence       `json:"echo_sequence"`
	KVSequence     materializeSequence       `json:"kv_sequence"`
	ReplaySequence materializeReplayVector   `json:"replay_sequence"`
}

type materializeDigestVector struct {
	Request materializeDigestRequest `json:"request"`
	Digest  string                   `json:"digest"`
}

type materializeDigestRequest struct {
	AppendIntentID            string   `json:"append_intent_id"`
	TargetTraceOwnerID        string   `json:"target_trace_owner_id"`
	TargetRecordIDs           []string `json:"target_record_ids"`
	TargetThroughOwnerOrdinal int64    `json:"target_through_owner_ordinal"`
	CaptureTraceOwnerID       *string  `json:"capture_trace_owner_id"`
	ActorRef                  string   `json:"actor_ref"`
	AuthorityRefs             []string `json:"authority_refs"`
	SchemaEnvironmentRef      string   `json:"schema_environment_ref"`
	TrustMode                 *string  `json:"trust_mode"`
}

type materializeSequence struct {
	Declaration materializeDeclaration   `json:"declaration"`
	Request     materializeSeqRequest    `json:"request"`
	Receipt     materializeSeqReceipt    `json:"receipt"`
	Capture     materializeRecordVector  `json:"capture"`
	Witness     materializeWitnessVector `json:"witness"`
	WorldValue  any                      `json:"world_value"`
}

type materializeDeclaration struct {
	Intent       string                   `json:"intent"`
	Owner        string                   `json:"owner"`
	SubstrateRef string                   `json:"substrate_ref"`
	FactIDs      []string                 `json:"fact_ids"`
	Drafts       []materializeDraftVector `json:"drafts"`
}

type materializeDraftVector struct {
	Mode      string         `json:"mode"`
	SchemaRef string         `json:"schema_ref"`
	KindLabel string         `json:"kind_label"`
	Payload   map[string]any `json:"payload"`
}

type materializeSeqRequest struct {
	AppendIntentID            string  `json:"append_intent_id"`
	TargetTraceOwnerID        string  `json:"target_trace_owner_id"`
	TargetThroughOwnerOrdinal int64   `json:"target_through_owner_ordinal"`
	CaptureTraceOwnerID       *string `json:"capture_trace_owner_id"`
}

type materializeSeqReceipt struct {
	Outcome           string           `json:"outcome"`
	SubstrateRef      string           `json:"substrate_ref"`
	TargetRecordIDs   []string         `json:"target_record_ids"`
	ProducedRecordIDs []string         `json:"produced_record_ids"`
	FailureReason     string           `json:"failure_reason"`
	WorldSideAnchors  []map[string]any `json:"world_side_anchors"`
}

type materializeRecordVector struct {
	RecordID   string         `json:"record_id"`
	Digest     string         `json:"digest"`
	SchemaRef  string         `json:"schema_ref"`
	Mode       string         `json:"mode"`
	WitnessRef string         `json:"witness_ref"`
	CausedBy   []string       `json:"caused_by"`
	KindLabel  string         `json:"kind_label"`
	Payload    map[string]any `json:"payload"`
}

type materializeWitnessVector struct {
	RecordID     string `json:"record_id"`
	SubstrateRef string `json:"substrate_ref"`
	Containment  string `json:"containment"`
}

type materializeReplayVector struct {
	DeclarationFactIDs []string              `json:"declaration_fact_ids"`
	FirstReceipt       materializeSeqReceipt `json:"first_receipt"`
	SecondReceipt      materializeSeqReceipt `json:"second_receipt"`
	SubstrateCalls     int                   `json:"substrate_calls"`
}

func loadMaterializeVectors(t *testing.T) materializeVectorDoc {
	t.Helper()
	data, err := os.ReadFile("testdata/materialize_vectors_v0.json")
	if err != nil {
		t.Fatalf("read materialize vectors: %v", err)
	}
	var doc materializeVectorDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("parse materialize vectors: %v", err)
	}
	return doc
}

// TestMaterializeVectorsCarryProvenance pins the vector file's header the way
// TestCanonicalEdgeVectorsMatchPython pins the edge corpus: a regeneration
// that loses its traceable revision must fail loudly rather than re-pin
// silently. The pinned hash in golden_provenance_test.go guards the bytes;
// this guards the provenance the bytes claim.
func TestMaterializeVectorsCarryProvenance(t *testing.T) {
	doc := loadMaterializeVectors(t)
	if doc.Generator != "testdata/generate_materialize_vectors.py" {
		t.Errorf("generator = %q, want the committed generator", doc.Generator)
	}
	// HasPrefix, not ==: the generator's git_commit swallows failures and
	// returns "unknown (<exc>)", which an equality check would let through.
	if doc.SourceCommit == "" || strings.HasPrefix(doc.SourceCommit, "unknown") {
		t.Error("vector file has no source_commit, so the vectors cannot be traced to a revision")
	}
	if doc.SourceRepo == "" {
		t.Error("vector file has no source_repo, so the checkout that produced it is unrecorded")
	}
	if doc.PythonVersion == "" {
		t.Error("vector file has no python_version, so the generating interpreter is unrecorded")
	}
}

// TestMaterializeRequestDigestMatchesPython pins the digest algorithm: the
// ASCII json.dumps flavour, the exact payload keys, and the "" <-> null
// mapping for the Optional fields — including a non-ASCII intent id, a null
// trust mode, and an authority ref with characters Go's encoding/json would
// escape differently.
func TestMaterializeRequestDigestMatchesPython(t *testing.T) {
	doc := loadMaterializeVectors(t)
	for _, v := range doc.RequestDigests {
		req := MaterializationRequest{
			AppendIntentID:            v.Request.AppendIntentID,
			TargetTraceOwnerID:        v.Request.TargetTraceOwnerID,
			TargetRecordIDs:           v.Request.TargetRecordIDs,
			TargetThroughOwnerOrdinal: int(v.Request.TargetThroughOwnerOrdinal),
		}
		if v.Request.CaptureTraceOwnerID != nil {
			req.CaptureTraceOwnerID = *v.Request.CaptureTraceOwnerID
		}
		ctx := OperationContext{
			ActorRef:               v.Request.ActorRef,
			Operation:              OpMaterialize,
			PresentedAuthorityRefs: v.Request.AuthorityRefs,
			SchemaEnvironmentRef:   v.Request.SchemaEnvironmentRef,
		}
		if v.Request.TrustMode != nil {
			ctx.TrustMode = *v.Request.TrustMode
		}
		got, err := materializationRequestDigest(req, ctx)
		if err != nil {
			t.Fatalf("digest(%q): %v", v.Request.AppendIntentID, err)
		}
		if got != v.Digest {
			t.Errorf("digest(%q) = %s, want %s", v.Request.AppendIntentID, got, v.Digest)
		}
	}
}

// TestMaterializeEchoSequenceMatchesPython replays the echo sequence through
// the Go dispatch and requires the same declaration ids, the same produced
// capture id, the same receipt, and the same retained content — including
// the witness the captures cite.
func TestMaterializeEchoSequenceMatchesPython(t *testing.T) {
	doc := loadMaterializeVectors(t)
	seq := doc.EchoSequence
	store := newMemStore(t)
	registry := NewSubstrateRegistry()
	if err := registry.Register(NewEchoSubstrate(seq.Declaration.SubstrateRef, seq.Declaration.Drafts[0].SchemaRef)); err != nil {
		t.Fatalf("register: %v", err)
	}

	declaration, err := store.Append(vectorAppendContext(), vectorDeclarationBatch(seq.Declaration))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	if !reflect.DeepEqual(declaration.FactIDs, seq.Declaration.FactIDs) {
		t.Errorf("declaration ids = %v, want %v", declaration.FactIDs, seq.Declaration.FactIDs)
	}

	receipt, err := Materialize(context.Background(), store, vectorMaterializeContext(), vectorRequest(seq.Request, declaration.FactIDs), registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	assertReceiptEquals(t, receipt, seq.Receipt)
	assertCaptureEquals(t, store, receipt.ProducedRecordIDs[0], seq.Capture, seq.Witness)
}

// TestMaterializeKVSequenceMatchesPython replays the KV sequence, including
// the world-side value the substrate must have written and the anchors the
// receipt must carry.
func TestMaterializeKVSequenceMatchesPython(t *testing.T) {
	doc := loadMaterializeVectors(t)
	seq := doc.KVSequence
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

	declaration, err := store.Append(vectorAppendContext(), vectorDeclarationBatch(seq.Declaration))
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	if !reflect.DeepEqual(declaration.FactIDs, seq.Declaration.FactIDs) {
		t.Errorf("declaration ids = %v, want %v", declaration.FactIDs, seq.Declaration.FactIDs)
	}

	receipt, err := Materialize(context.Background(), store, vectorMaterializeContext(), vectorRequest(seq.Request, declaration.FactIDs), registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	assertReceiptEquals(t, receipt, seq.Receipt)
	assertCaptureEquals(t, store, receipt.ProducedRecordIDs[0], seq.Capture, seq.Witness)

	world, err := kv.Get("answer")
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	if !reflect.DeepEqual(world, seq.WorldValue) {
		t.Errorf("world value = %v (%T), want %v", world, world, seq.WorldValue)
	}
}

// TestMaterializeLedgerReplayAcrossRestartMatchesPython replays the
// restart sequence: the ledger replays the stored receipt after the store is
// closed and reopened, and the substrate is dispatched exactly once.
func TestMaterializeLedgerReplayAcrossRestartMatchesPython(t *testing.T) {
	doc := loadMaterializeVectors(t)
	seq := doc.ReplaySequence

	dbPath := filepath.Join(t.TempDir(), "trace.sqlite")
	substrate := &vectorCountingSubstrate{}
	registry := NewSubstrateRegistry()
	if err := registry.Register(substrate); err != nil {
		t.Fatalf("register: %v", err)
	}

	store, err := NewSQLiteTraceStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	declaration, err := store.Append(vectorAppendContext(), AppendBatch{
		AppendIntentID: "vector:replay:declare",
		Groups: []AppendGroup{{
			TraceOwnerID: "owner:vector-replay",
			RetainedContext: &RetainedContext{
				SubstrateRef: "vector.count.v1",
				Containment:  ContainContained,
			},
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: "vector.write.v1",
				KindLabel: "write",
				Payload:   map[string]any{"value": jsonNumber(t, 1)},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	if !reflect.DeepEqual(declaration.FactIDs, seq.DeclarationFactIDs) {
		t.Errorf("declaration ids = %v, want %v", declaration.FactIDs, seq.DeclarationFactIDs)
	}
	req := MaterializationRequest{
		AppendIntentID:            "vector:replay:apply",
		TargetTraceOwnerID:        "owner:vector-replay",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: MaxOwnerOrdinal,
	}

	first, err := Materialize(context.Background(), store, vectorMaterializeContext(), req, registry)
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	store.Close()

	restarted, err := NewSQLiteTraceStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer restarted.Close()
	second, err := Materialize(context.Background(), restarted, vectorMaterializeContext(), req, registry)
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}

	assertReceiptEquals(t, first, seq.FirstReceipt)
	assertReceiptEquals(t, second, seq.SecondReceipt)
	if substrate.calls != seq.SubstrateCalls {
		t.Errorf("substrate calls = %d, want %d", substrate.calls, seq.SubstrateCalls)
	}
}

// --- replay helpers ---

// vectorCountingSubstrate mirrors the generator's _CountingSubstrate: same
// ref, same schemas, same emitted payload, so the replayed receipts are
// comparable id-for-id.
type vectorCountingSubstrate struct {
	calls int
}

func (c *vectorCountingSubstrate) SubstrateRef() string         { return "vector.count.v1" }
func (c *vectorCountingSubstrate) DeclarationSchemas() []string { return []string{"vector.write.v1"} }
func (c *vectorCountingSubstrate) CaptureSchemas() []string {
	return []string{"vector.write.applied.v1"}
}
func (c *vectorCountingSubstrate) Containment() Containment { return ContainContained }

func (c *vectorCountingSubstrate) Materialize(_ context.Context, records []Record) (MaterializationResult, error) {
	c.calls++
	return MaterializationResult{
		Outcome: MaterializationSuccess,
		CaptureDrafts: []RecordDraft{{
			Mode:            Capture,
			SchemaRef:       "vector.write.applied.v1",
			KindLabel:       "write_applied",
			Payload:         map[string]any{"call": c.calls},
			CausedByFactIDs: []string{records[0].Envelope.RecordID},
		}},
	}, nil
}

// vectorAppendContext mirrors the generator's APPEND context.
func vectorAppendContext() AppendContext {
	return AppendContext{
		ActorRef:             "runtime:vector",
		PresentedWitnessRefs: []string{"trusted:internal"},
		SchemaVersionSet:     "shepherd2-slice-a",
		TrustMode:            "internal",
	}
}

// vectorMaterializeContext mirrors the generator's MATERIALIZE context.
func vectorMaterializeContext() OperationContext {
	return vectorAppendContext().ToOperationContext(OpMaterialize)
}

func vectorDeclarationBatch(decl materializeDeclaration) AppendBatch {
	drafts := make([]RecordDraft, 0, len(decl.Drafts))
	for _, d := range decl.Drafts {
		drafts = append(drafts, RecordDraft{
			Mode:      RecordMode(d.Mode),
			SchemaRef: d.SchemaRef,
			KindLabel: d.KindLabel,
			Payload:   d.Payload,
		})
	}
	return AppendBatch{
		AppendIntentID: decl.Intent,
		Groups: []AppendGroup{{
			TraceOwnerID: decl.Owner,
			RetainedContext: &RetainedContext{
				SubstrateRef: decl.SubstrateRef,
				Containment:  ContainContained,
			},
			FactDrafts: drafts,
		}},
	}
}

func vectorRequest(req materializeSeqRequest, factIDs []string) MaterializationRequest {
	out := MaterializationRequest{
		AppendIntentID:            req.AppendIntentID,
		TargetTraceOwnerID:        req.TargetTraceOwnerID,
		TargetRecordIDs:           factIDs,
		TargetThroughOwnerOrdinal: int(req.TargetThroughOwnerOrdinal),
	}
	if req.CaptureTraceOwnerID != nil {
		out.CaptureTraceOwnerID = *req.CaptureTraceOwnerID
	}
	return out
}

func assertReceiptEquals(t *testing.T, got MaterializationReceipt, want materializeSeqReceipt) {
	t.Helper()
	if string(got.Outcome) != want.Outcome {
		t.Errorf("receipt outcome = %q, want %q", got.Outcome, want.Outcome)
	}
	if got.SubstrateRef != want.SubstrateRef {
		t.Errorf("receipt substrate_ref = %q, want %q", got.SubstrateRef, want.SubstrateRef)
	}
	if !reflect.DeepEqual(got.TargetRecordIDs, want.TargetRecordIDs) {
		t.Errorf("receipt target ids = %v, want %v", got.TargetRecordIDs, want.TargetRecordIDs)
	}
	if !reflect.DeepEqual(got.ProducedRecordIDs, want.ProducedRecordIDs) {
		t.Errorf("receipt produced ids = %v, want %v", got.ProducedRecordIDs, want.ProducedRecordIDs)
	}
	if got.FailureReason != want.FailureReason {
		t.Errorf("receipt failure_reason = %q, want %q", got.FailureReason, want.FailureReason)
	}
	if !reflect.DeepEqual(got.WorldSideAnchors, want.WorldSideAnchors) {
		t.Errorf("receipt anchors = %v, want %v", got.WorldSideAnchors, want.WorldSideAnchors)
	}
}

func assertCaptureEquals(t *testing.T, store *SQLiteTraceStore, factID string, want materializeRecordVector, wantWitness materializeWitnessVector) {
	t.Helper()
	visible, err := store.ReadFact(reader, factID)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	rec, ok := visible.(Record)
	if !ok {
		t.Fatalf("capture is a shape, not a record: %T", visible)
	}
	if rec.Envelope.RecordID != want.RecordID || rec.Envelope.Digest != want.Digest {
		t.Errorf("capture identity = %s/%s, want %s/%s", rec.Envelope.RecordID, rec.Envelope.Digest, want.RecordID, want.Digest)
	}
	if rec.Envelope.SchemaRef != want.SchemaRef || string(rec.Envelope.Mode) != want.Mode {
		t.Errorf("capture = %q/%q, want %q/%q", rec.Envelope.SchemaRef, rec.Envelope.Mode, want.SchemaRef, want.Mode)
	}
	if rec.Envelope.WitnessRef != want.WitnessRef {
		t.Errorf("capture witness_ref = %q, want %q", rec.Envelope.WitnessRef, want.WitnessRef)
	}
	if !reflect.DeepEqual(rec.Envelope.CausedByIDs, want.CausedBy) {
		t.Errorf("capture caused_by = %v, want %v", rec.Envelope.CausedByIDs, want.CausedBy)
	}
	if kind := rec.View.KindLabel; kind != want.KindLabel {
		t.Errorf("capture kind = %q, want %q", kind, want.KindLabel)
	}
	if !reflect.DeepEqual(rec.Body.Payload, want.Payload) {
		t.Errorf("capture payload = %v, want %v", rec.Body.Payload, want.Payload)
	}

	witnessVisible, err := store.ReadFact(reader, rec.Envelope.WitnessRef)
	if err != nil {
		t.Fatalf("read witness: %v", err)
	}
	witness, ok := witnessVisible.(Record)
	if !ok {
		t.Fatalf("witness is a shape, not a record: %T", witnessVisible)
	}
	if witness.Envelope.RecordID != wantWitness.RecordID {
		t.Errorf("witness id = %q, want %q", witness.Envelope.RecordID, wantWitness.RecordID)
	}
	if witness.Body.Payload["substrate_ref"] != wantWitness.SubstrateRef {
		t.Errorf("witness substrate_ref = %v, want %q", witness.Body.Payload["substrate_ref"], wantWitness.SubstrateRef)
	}
	if witness.Body.Payload["containment"] != wantWitness.Containment {
		t.Errorf("witness containment = %v, want %q", witness.Body.Payload["containment"], wantWitness.Containment)
	}
}
