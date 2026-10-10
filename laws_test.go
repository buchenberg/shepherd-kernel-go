package shepherd

// ABI law tests (T1.8, plan 01 §6), ported from
// shepherd2/tests/test_kernel_abi_v0_laws.py — 25 executable laws that pin the
// kernel ABI. The law → test mapping, including the laws covered elsewhere and
// the documented N/A rationale, lives in docs/law-coverage.md; this file holds
// the laws that had no Go home.
//
// The store fixes these laws required are in store.go: duplicate causal
// parents are rejected rather than deduplicated (resolvedCauses), the ordinary
// witness plan validates its body before digesting it (ordinaryWitnessPlan),
// and closure reads keep the SQL sort order instead of ranging over a map
// (canonicalFactOrder). All three divergences were found by porting the laws,
// not by inspection — the third was a per-process flake that only a repeated
// run could catch.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// lawAppend mirrors the Python suite's APPEND context.
var lawAppend = AppendContext{
	ActorRef:             "runtime:laws",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

// lawReader mirrors the Python suite's READ context.
var lawReader = ReadContext{ActorRef: "reader:laws", VisibilityProfile: VisibilityPayload}

// lawShapeReader reads with shape_only visibility.
var lawShapeReader = ReadContext{ActorRef: "reader", VisibilityProfile: VisibilityShapeOnly}

// lawAppendDrafts appends one group under one intent with the law context.
func lawAppendDrafts(t *testing.T, store *SQLiteTraceStore, intent, owner string, drafts ...RecordDraft) AppendReceipt {
	t.Helper()
	receipt, err := store.Append(context.Background(), lawAppend, AppendBatch{
		AppendIntentID: intent,
		Groups:         []AppendGroup{{TraceOwnerID: owner, FactDrafts: drafts}},
	})
	if err != nil {
		t.Fatalf("Append(%s): %v", intent, err)
	}
	return receipt
}

func wantErrorContaining(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error %q does not contain %q", err.Error(), substr)
	}
}

// TestLawRecordDigestExcludesLegacyKindLabel ports
// test_record_digest_excludes_legacy_kind_label: two drafts differing only in
// kind label produce the same record id, and each owner's read keeps its own
// label — the label is view metadata, never digest input.
func TestLawRecordDigestExcludesLegacyKindLabel(t *testing.T) {
	store := newMemStore(t)
	// The schema ref is identical; only the kind label differs. (The draft()
	// helper would fold the label into the schema ref, which would make this
	// law trivially pass for the wrong reason.)
	firstDraft := RecordDraft{
		KindLabel: "friendly_name",
		Mode:      Capture,
		SchemaRef: "law.same_schema.v1",
		Payload:   map[string]any{"value": 1},
	}
	secondDraft := RecordDraft{
		KindLabel: "renamed_view_label",
		Mode:      Capture,
		SchemaRef: "law.same_schema.v1",
		Payload:   map[string]any{"value": 1},
	}
	first := lawAppendDrafts(t, store, "law:label:first", "owner:label:first", firstDraft)
	second := lawAppendDrafts(t, store, "law:label:second", "owner:label:second", secondDraft)
	if first.FactIDs[0] != second.FactIDs[0] {
		t.Errorf("kind label leaked into the digest: %q vs %q", first.FactIDs[0], second.FactIDs[0])
	}

	firstSlice, err := store.ReadOwnerPrefix(context.Background(), lawReader, "owner:label:first", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read owner:label:first: %v", err)
	}
	secondSlice, err := store.ReadOwnerPrefix(context.Background(), lawReader, "owner:label:second", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read owner:label:second: %v", err)
	}
	firstFact, ok := firstSlice.FactsByID[first.FactIDs[0]].(Record)
	if !ok || firstFact.View == nil {
		t.Fatalf("owner:label:first: expected a retained Record with a view")
	}
	secondFact, ok := secondSlice.FactsByID[second.FactIDs[0]].(Record)
	if !ok || secondFact.View == nil {
		t.Fatalf("owner:label:second: expected a retained Record with a view")
	}
	if firstFact.View.KindLabel != "friendly_name" {
		t.Errorf("owner:label:first kind = %q, want \"friendly_name\"", firstFact.View.KindLabel)
	}
	if secondFact.View.KindLabel != "renamed_view_label" {
		t.Errorf("owner:label:second kind = %q, want \"renamed_view_label\"", secondFact.View.KindLabel)
	}
}

// TestLawWitnessRecordsAreRetainedAndNonRootRecordsHaveWitnesses ports
// test_witness_records_are_retained_and_non_root_records_have_witnesses.
func TestLawWitnessRecordsAreRetainedAndNonRootRecordsHaveWitnesses(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:witness", "owner:witness", draft("step", Capture, nil))

	recordVisible, err := store.ReadFact(context.Background(), lawReader, receipt.FactIDs[0])
	if err != nil {
		t.Fatalf("ReadFact(record): %v", err)
	}
	record, ok := recordVisible.(Record)
	if !ok {
		t.Fatalf("record is %T, want Record", recordVisible)
	}
	rootVisible, err := store.ReadFact(context.Background(), lawReader, RootWitnessRecordIDMust())
	if err != nil {
		t.Fatalf("ReadFact(root): %v", err)
	}
	root, ok := rootVisible.(Record)
	if !ok {
		t.Fatalf("root witness is %T, want Record", rootVisible)
	}

	if record.Envelope.WitnessRef == "" {
		t.Error("non-root record must carry a witness ref")
	}
	if root.Envelope.SchemaRef != RootWitnessSchemaRef {
		t.Errorf("root schema_ref = %q, want %q", root.Envelope.SchemaRef, RootWitnessSchemaRef)
	}
	if root.Envelope.WitnessRef != RootWitnessRef {
		t.Errorf("root witness_ref = %q, want the empty sentinel", root.Envelope.WitnessRef)
	}

	witnessVisible, err := store.ReadFact(context.Background(), lawReader, record.Envelope.WitnessRef)
	if err != nil {
		t.Fatalf("ReadFact(witness): %v", err)
	}
	witness, ok := witnessVisible.(Record)
	if !ok {
		t.Fatalf("witness is %T, want Record", witnessVisible)
	}
	// A record cites the witness *record* id, never the witness *body* digest.
	bodyDigest, err := WitnessBodyDigest(witness.Envelope.SchemaRef, witness.Body.Payload)
	if err != nil {
		t.Fatalf("WitnessBodyDigest: %v", err)
	}
	if record.Envelope.WitnessRef == bodyDigest {
		t.Error("record cites the witness body digest, want the witness record id")
	}
	if len(witness.Envelope.CausedByIDs) != 0 {
		t.Errorf("witness has causal parents %v, want none", witness.Envelope.CausedByIDs)
	}
	if len(root.Envelope.CausedByIDs) != 0 {
		t.Errorf("root witness has causal parents %v, want none", root.Envelope.CausedByIDs)
	}
}

// TestLawWitnessBodyValidationIsEnforcedOnAppendAndPreview ports
// test_witness_body_validation_is_enforced_on_append_and_preview.
func TestLawWitnessBodyValidationIsEnforcedOnAppendAndPreview(t *testing.T) {
	store := newMemStore(t)
	owner := "owner:witness-validation"
	emptySubstrate := AppendBatch{
		AppendIntentID: "law:witness:empty-substrate",
		Groups: []AppendGroup{{
			TraceOwnerID:    owner,
			RetainedContext: &RetainedContext{SubstrateRef: "", Containment: ContainContained},
			FactDrafts:      []RecordDraft{draft("step", Capture, nil)},
		}},
	}
	invalidContainment := AppendBatch{
		AppendIntentID: "law:witness:invalid-containment",
		Groups: []AppendGroup{{
			TraceOwnerID:    owner,
			RetainedContext: &RetainedContext{SubstrateRef: "sqlite.local.v1", Containment: Containment("partial")},
			FactDrafts:      []RecordDraft{draft("step", Capture, nil)},
		}},
	}

	_, err := store.Append(context.Background(), lawAppend, emptySubstrate)
	wantErrorContaining(t, err, "substrate_ref")
	_, err = store.PreviewRecordIDs(context.Background(), lawAppend, emptySubstrate)
	wantErrorContaining(t, err, "substrate_ref")
	_, err = store.Append(context.Background(), lawAppend, invalidContainment)
	wantErrorContaining(t, err, "containment")
	_, err = store.PreviewRecordIDs(context.Background(), lawAppend, invalidContainment)
	wantErrorContaining(t, err, "containment")

	// An unknown but non-empty substrate is retained verbatim: the kernel
	// requires a substrate to be named, not that it be a known one.
	unknownSubstrate := AppendBatch{
		AppendIntentID: "law:witness:unknown-substrate",
		Groups: []AppendGroup{{
			TraceOwnerID:    owner,
			RetainedContext: &RetainedContext{SubstrateRef: "unknown.substrate.v1", Containment: ContainContained},
			FactDrafts:      []RecordDraft{draft("step", Capture, nil)},
		}},
	}
	receipt, err := store.Append(context.Background(), lawAppend, unknownSubstrate)
	if err != nil {
		t.Fatalf("Append(unknown substrate): %v", err)
	}
	retained, err := store.ReadFact(context.Background(), lawReader, receipt.FactIDs[0])
	if err != nil {
		t.Fatalf("ReadFact: %v", err)
	}
	fact, ok := retained.(Record)
	if !ok {
		t.Fatalf("retained record is %T, want Record", retained)
	}
	witnessVisible, err := store.ReadFact(context.Background(), lawReader, fact.Envelope.WitnessRef)
	if err != nil {
		t.Fatalf("ReadFact(witness): %v", err)
	}
	witness, ok := witnessVisible.(Record)
	if !ok {
		t.Fatalf("witness is %T, want Record", witnessVisible)
	}
	if got := witness.Body.Payload["substrate_ref"]; got != "unknown.substrate.v1" {
		t.Errorf("witness substrate_ref = %v, want \"unknown.substrate.v1\"", got)
	}
}

// TestLawNonRootRecordWithEmptyWitnessRefRejected ports
// test_non_root_record_with_empty_witness_ref_rejected.
func TestLawNonRootRecordWithEmptyWitnessRefRejected(t *testing.T) {
	_, err := CanonicalRecordInput("law.non_root.v1", Capture, map[string]any{}, nil, "")
	wantErrorContaining(t, err, "root witness")
	_, err = CanonicalRecordInput(WitnessSchemaRef, Capture, map[string]any{}, nil, "")
	wantErrorContaining(t, err, "root witness")
}

// TestLawOrderedCausalityRejectsDuplicateParents ports
// test_ordered_causality_rejects_duplicate_parents.
func TestLawOrderedCausalityRejectsDuplicateParents(t *testing.T) {
	store := newMemStore(t)
	parent := lawAppendDrafts(t, store, "law:parent", "owner:causal", draft("parent", Capture, nil))

	_, err := store.Append(context.Background(), lawAppend, AppendBatch{
		AppendIntentID: "law:duplicate-parent",
		Groups: []AppendGroup{{
			TraceOwnerID:  "owner:causal",
			CausalParents: parent.FactIDs,
			FactDrafts:    []RecordDraft{draft("child", Capture, nil, parent.FactIDs...)},
		}},
	})
	wantErrorContaining(t, err, "duplicate causal parent")
}

// TestLawAppendLocalRefsAreResolvedBeforeRetention ports
// test_append_local_refs_are_resolved_before_retention.
func TestLawAppendLocalRefsAreResolvedBeforeRetention(t *testing.T) {
	store := newMemStore(t)
	receipt, err := store.Append(context.Background(), lawAppend, AppendBatch{
		AppendIntentID: "law:local-refs",
		Groups: []AppendGroup{{
			TraceOwnerID: "owner:local-refs",
			FactDrafts: []RecordDraft{
				{
					KindLabel:     "parent",
					Mode:          Capture,
					SchemaRef:     "law.parent.v1",
					AppendLocalID: "local:parent",
					Payload:       nil,
				},
				{
					KindLabel:         "child",
					Mode:              Capture,
					SchemaRef:         "law.child.v1",
					CausedByLocalRefs: []string{"local:parent"},
					Payload:           nil,
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	childVisible, err := store.ReadFact(context.Background(), lawReader, receipt.FactIDs[1])
	if err != nil {
		t.Fatalf("ReadFact(child): %v", err)
	}
	child, ok := childVisible.(Record)
	if !ok {
		t.Fatalf("child is %T, want Record", childVisible)
	}
	want := []string{receipt.FactIDs[0]}
	if len(child.Envelope.CausedByIDs) != len(want) {
		t.Fatalf("child caused_by = %v, want %v", child.Envelope.CausedByIDs, want)
	}
	for i, id := range want {
		if child.Envelope.CausedByIDs[i] != id {
			t.Errorf("child caused_by[%d] = %q, want %q", i, child.Envelope.CausedByIDs[i], id)
		}
	}
	for _, id := range child.Envelope.CausedByIDs {
		if strings.HasPrefix(id, "local:") {
			t.Errorf("local ref %q leaked into the retained causal parents", id)
		}
	}
}

// TestLawShapeOnlyHidesPayloadsAndPreservesContextAnchor ports
// test_shape_only_hides_payloads_and_preserves_context_anchor.
func TestLawShapeOnlyHidesPayloadsAndPreservesContextAnchor(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:shape-only", "owner:shape-only",
		draft("step", Capture, map[string]any{"secret": true}),
	)
	cut, err := store.PublishCut(context.Background(), lawAppend, FrontierSpec{
		FrontierID:         "law:cut:shape-only",
		TargetTraceOwnerID: "owner:shape-only",
		ThroughFactID:      receipt.FactIDs[len(receipt.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	view, err := store.ResolveCut(context.Background(), lawShapeReader, cut.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut(shape_only): %v", err)
	}
	visible, ok := view.FactsByID[receipt.FactIDs[0]].(RecordShape)
	if !ok {
		t.Fatalf("visible fact is %T, want RecordShape under shape_only", view.FactsByID[receipt.FactIDs[0]])
	}
	if visible.HiddenReason == "" {
		t.Error("RecordShape should name why the payload is hidden")
	}
	if len(view.ContextsByID) != 0 {
		t.Errorf("shape_only exposed %d retained contexts, want 0", len(view.ContextsByID))
	}
	if len(view.ContextAnchors) == 0 {
		t.Error("shape_only must preserve a context anchor")
	}
	if len(view.WitnessesByID) != 0 {
		t.Errorf("shape_only exposed %d witnesses, want 0", len(view.WitnessesByID))
	}
	if len(view.WitnessAnchors) == 0 {
		t.Error("shape_only must preserve witness anchors")
	}
}

// TestLawSliceExposesVisibleWitnessRecordsUnderPayloadVisibility ports
// test_slice_exposes_visible_witness_records_under_payload_visibility.
func TestLawSliceExposesVisibleWitnessRecordsUnderPayloadVisibility(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:visible-witnesses", "owner:visible-witnesses", draft("step", Capture, nil))

	view, err := store.ReadOwnerPrefix(context.Background(), lawReader, "owner:visible-witnesses", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	visible, ok := view.FactsByID[receipt.FactIDs[0]].(Record)
	if !ok {
		t.Fatalf("visible fact is %T, want Record", view.FactsByID[receipt.FactIDs[0]])
	}

	witness, ok := view.WitnessesByID[visible.Envelope.WitnessRef].(Record)
	if !ok {
		t.Fatalf("witness %s missing from the slice", visible.Envelope.WitnessRef)
	}
	if witness.Envelope.SchemaRef != WitnessSchemaRef {
		t.Errorf("witness schema_ref = %q, want %q", witness.Envelope.SchemaRef, WitnessSchemaRef)
	}
	if got := witness.Body.Payload["actor_ref"]; got != "runtime:laws" {
		t.Errorf("witness actor_ref = %v, want \"runtime:laws\"", got)
	}
	root, ok := view.WitnessesByID[RootWitnessRecordIDMust()].(Record)
	if !ok {
		t.Fatalf("root witness missing from the slice")
	}
	if root.Envelope.SchemaRef != RootWitnessSchemaRef {
		t.Errorf("root schema_ref = %q, want %q", root.Envelope.SchemaRef, RootWitnessSchemaRef)
	}
	if len(view.WitnessAnchors) != 0 {
		t.Errorf("payload visibility produced %d witness anchors, want 0", len(view.WitnessAnchors))
	}
}

// TestLawSliceWitnessSupportIsClosedToRootUnderShapeVisibility ports
// test_slice_witness_support_is_closed_to_root_under_shape_visibility.
func TestLawSliceWitnessSupportIsClosedToRootUnderShapeVisibility(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:witness-closure-shape", "owner:witness-closure-shape", draft("step", Capture, nil))

	view, err := store.ReadOwnerPrefix(context.Background(), lawShapeReader, "owner:witness-closure-shape", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(shape_only): %v", err)
	}
	visible, ok := view.FactsByID[receipt.FactIDs[0]].(RecordShape)
	if !ok {
		t.Fatalf("visible fact is %T, want RecordShape under shape_only", view.FactsByID[receipt.FactIDs[0]])
	}
	if len(view.WitnessesByID) != 0 {
		t.Errorf("shape_only exposed %d witnesses, want 0", len(view.WitnessesByID))
	}

	refs := make(map[string]bool)
	for _, anchor := range view.WitnessAnchors {
		refs[anchor.WitnessRef] = true
	}
	if !refs[visible.Envelope.WitnessRef] {
		t.Errorf("witness anchors %v lack the record's own witness", refs)
	}
	if !refs[RootWitnessRecordIDMust()] {
		t.Errorf("witness anchors %v are not closed to the root witness", refs)
	}
}

// TestLawWitnessSupportRejectsWitnessCycles ports
// test_witness_support_rejects_witness_cycles.
//
// The cycle is induced by direct SQL, the way the Python law does it: this is
// a corruption-detection law, not an API law — no append path can produce it.
func TestLawWitnessSupportRejectsWitnessCycles(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:witness-cycle", "owner:witness-cycle", draft("step", Capture, nil))
	recordVisible, err := store.ReadFact(context.Background(), lawReader, receipt.FactIDs[0])
	if err != nil {
		t.Fatalf("ReadFact: %v", err)
	}
	witnessRef := recordVisible.(Record).Envelope.WitnessRef

	if _, err := store.db.Exec(
		"UPDATE records SET witness_ref = ? WHERE record_id = ?",
		witnessRef, witnessRef,
	); err != nil {
		t.Fatalf("induce witness cycle: %v", err)
	}

	_, err = store.ReadOwnerPrefix(context.Background(), lawReader, "owner:witness-cycle", 99, ModeBoth)
	wantErrorContaining(t, err, "cycle")
}

// TestLawShapeOnlyWitnessAnchorPreservesWitnessRefShape ports
// test_shape_only_witness_anchor_preserves_witness_ref_shape: each anchor's
// shape carries its own witness_ref, so the chain stays readable while the
// witness bodies stay hidden.
func TestLawShapeOnlyWitnessAnchorPreservesWitnessRefShape(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:witness-anchor-shape", "owner:witness-anchor-shape", draft("step", Capture, nil))

	view, err := store.ReadOwnerPrefix(context.Background(), lawShapeReader, "owner:witness-anchor-shape", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(shape_only): %v", err)
	}
	visible, ok := view.FactsByID[receipt.FactIDs[0]].(RecordShape)
	if !ok {
		t.Fatalf("visible fact is %T, want RecordShape under shape_only", view.FactsByID[receipt.FactIDs[0]])
	}

	var ordinary, root *WitnessAnchor
	for i := range view.WitnessAnchors {
		anchor := &view.WitnessAnchors[i]
		switch anchor.WitnessRef {
		case visible.Envelope.WitnessRef:
			ordinary = anchor
		case RootWitnessRecordIDMust():
			root = anchor
		}
	}
	if ordinary == nil {
		t.Fatal("no anchor for the ordinary witness")
	}
	if root == nil {
		t.Fatal("no anchor for the root witness")
	}
	if got := ordinary.VisibleShape["witness_ref"]; got != RootWitnessRecordIDMust() {
		t.Errorf("ordinary witness anchor shape witness_ref = %v, want the root id", got)
	}
	if got := root.VisibleShape["witness_ref"]; got != "" {
		t.Errorf("root witness anchor shape witness_ref = %v, want the empty sentinel", got)
	}
}

// TestLawWitnessSupportIgnoresModeFilterAndDoesNotChangeSelectedGraph ports
// test_witness_support_ignores_mode_filter_and_does_not_change_selected_graph.
func TestLawWitnessSupportIgnoresModeFilterAndDoesNotChangeSelectedGraph(t *testing.T) {
	store := newMemStore(t)
	declaration := lawAppendDrafts(t, store, "law:witness-mode:declaration", "owner:witness-mode",
		draft("intent", Declaration, nil),
	)
	capture, err := store.Append(context.Background(), lawAppend, AppendBatch{
		AppendIntentID: "law:witness-mode:capture",
		Groups: []AppendGroup{{
			TraceOwnerID:  "owner:witness-mode",
			CausalParents: declaration.FactIDs,
			FactDrafts:    []RecordDraft{draft("capture", Capture, nil)},
		}},
	})
	if err != nil {
		t.Fatalf("Append(capture): %v", err)
	}

	view, err := store.ReadOwnerPrefix(context.Background(), lawReader, "owner:witness-mode", 99, ModeCapturesOnly)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(captures_only): %v", err)
	}
	if len(view.FactIDs()) != len(capture.FactIDs) {
		t.Fatalf("captures_only selected %d facts, want %d", len(view.FactIDs()), len(capture.FactIDs))
	}
	for i, id := range capture.FactIDs {
		if view.FactIDs()[i] != id {
			t.Errorf("captures_only fact[%d] = %q, want %q", i, view.FactIDs()[i], id)
		}
	}
	if len(view.OwnerPaths) != 1 || len(view.OwnerPaths["owner:witness-mode"]) != len(capture.FactIDs) {
		t.Errorf("owner paths = %v, want only owner:witness-mode with the capture", view.OwnerPaths)
	}
	if len(view.CausalEdges) != 0 {
		t.Errorf("causal edges = %v, want none (the declaration is not selected)", view.CausalEdges)
	}

	visible, ok := view.FactsByID[capture.FactIDs[0]].(Record)
	if !ok {
		t.Fatalf("capture is %T, want Record", view.FactsByID[capture.FactIDs[0]])
	}
	if len(view.WitnessesByID) != 2 {
		t.Errorf("witnesses = %v, want the capture's witness and the root", keysOf(view.WitnessesByID))
	}
	if _, ok := view.WitnessesByID[visible.Envelope.WitnessRef]; !ok {
		t.Errorf("witnesses %v lack the capture's own witness", keysOf(view.WitnessesByID))
	}
	if _, ok := view.WitnessesByID[RootWitnessRecordIDMust()]; !ok {
		t.Errorf("witnesses %v lack the root witness", keysOf(view.WitnessesByID))
	}

	found := false
	for _, anchor := range view.ExternalAnchors {
		if anchor.Ref == declaration.FactIDs[0] {
			found = true
		}
	}
	if !found {
		t.Errorf("external anchors lack the filtered-out declaration parent %s", declaration.FactIDs[0])
	}
}

// TestLawWitnessSupportIsDedupedByRecordID ports
// test_witness_support_is_deduped_by_record_id.
func TestLawWitnessSupportIsDedupedByRecordID(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:witness-dedupe", "owner:witness-dedupe",
		draft("first", Capture, nil),
		draft("second", Capture, nil),
	)

	view, err := store.ReadOwnerPrefix(context.Background(), lawReader, "owner:witness-dedupe", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	witnesses := make(map[string]bool)
	for _, visible := range view.FactsByID {
		if fact, ok := visible.(Record); ok && fact.Envelope.WitnessRef != "" {
			witnesses[fact.Envelope.WitnessRef] = true
		}
	}
	if len(witnesses) != 1 {
		t.Errorf("two facts in one group cite %d distinct witnesses, want 1", len(witnesses))
	}
	if len(view.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("fact ids = %v, want the receipt's", view.FactIDs())
	}
	for ref := range witnesses {
		if _, ok := view.WitnessesByID[ref]; !ok {
			t.Errorf("witness %s cited but not present in the slice", ref)
		}
	}
	if _, ok := view.WitnessesByID[RootWitnessRecordIDMust()]; !ok {
		t.Error("root witness missing from the slice")
	}
}

// TestLawMissingWitnessSupportFailsLoudly ports
// test_missing_witness_support_fails_loudly.
func TestLawMissingWitnessSupportFailsLoudly(t *testing.T) {
	store := newMemStore(t)
	missingWitnessRef := "sha256:" + strings.Repeat("0", 64)
	receipt := lawAppendDrafts(t, store, "law:missing-witness", "owner:missing-witness", draft("step", Capture, nil))

	if _, err := store.db.Exec(
		"UPDATE records SET witness_ref = ? WHERE record_id = ?",
		missingWitnessRef, receipt.FactIDs[0],
	); err != nil {
		t.Fatalf("point record at a missing witness: %v", err)
	}

	_, err := store.ReadOwnerPrefix(context.Background(), lawReader, "owner:missing-witness", 99, ModeBoth)
	if err == nil {
		t.Fatal("read with a dangling witness_ref succeeded, want a loud failure")
	}
	if !strings.Contains(err.Error(), missingWitnessRef) {
		t.Errorf("error %q does not name the missing witness ref", err.Error())
	}
}

// TestLawExternalAnchorPreservesOutOfCutParent ports
// test_external_anchor_preserves_out_of_cut_parent.
func TestLawExternalAnchorPreservesOutOfCutParent(t *testing.T) {
	store := newMemStore(t)
	child := lawAppendDrafts(t, store, "law:external-child", "owner:child", draft("child", Capture, nil))
	parent := lawAppendDrafts(t, store, "law:external-parent", "owner:parent",
		draft("parent", Capture, nil, child.FactIDs[0]),
	)
	cut, err := store.PublishCut(context.Background(), lawAppend, FrontierSpec{
		FrontierID:         "law:cut:external-parent",
		TargetTraceOwnerID: "owner:parent",
		ThroughFactID:      parent.FactIDs[len(parent.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	view, err := store.ResolveCut(context.Background(), lawReader, cut.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut: %v", err)
	}
	if len(view.FactIDs()) != len(parent.FactIDs) {
		t.Fatalf("cut has %d facts, want %d", len(view.FactIDs()), len(parent.FactIDs))
	}
	if len(view.ExternalAnchors) == 0 {
		t.Fatal("cut lost the out-of-cut causal parent")
	}
	if view.ExternalAnchors[0].Ref != child.FactIDs[0] {
		t.Errorf("external anchor ref = %q, want the child %q", view.ExternalAnchors[0].Ref, child.FactIDs[0])
	}
	if got := view.ExternalAnchors[0].VisibleShape["schema_ref"]; got != "shepherd2.conformance.child.v1" {
		t.Errorf("external anchor schema_ref = %v, want the child's schema", got)
	}
}

// TestLawCausalClosurePolicyControlsFilteredParentAnchors ports
// test_causal_closure_policy_controls_filtered_parent_anchors.
//
// The Python law's final sub-case — calling read_causal_closure without a
// closure policy is a TypeError — is not portable: Go's signature requires
// the argument, so the compiler rejects the call instead of the runtime.
func TestLawCausalClosurePolicyControlsFilteredParentAnchors(t *testing.T) {
	store := newMemStore(t)
	declaration := lawAppendDrafts(t, store, "law:closure-policy:declaration", "owner:closure-policy",
		draft("intent", Declaration, nil),
	)
	capture, err := store.Append(context.Background(), lawAppend, AppendBatch{
		AppendIntentID: "law:closure-policy:capture",
		Groups: []AppendGroup{{
			TraceOwnerID:  "owner:closure-policy",
			CausalParents: declaration.FactIDs,
			FactDrafts:    []RecordDraft{draft("capture", Capture, nil)},
		}},
	})
	if err != nil {
		t.Fatalf("Append(capture): %v", err)
	}

	anchored, err := store.ReadCausalClosure(context.Background(), lawReader, capture.FactIDs, ModeCapturesOnly, "include_external_anchors")
	if err != nil {
		t.Fatalf("ReadCausalClosure(include_external_anchors): %v", err)
	}
	if len(anchored.FactIDs()) != len(capture.FactIDs) {
		t.Errorf("anchored closure = %v, want only the capture", anchored.FactIDs())
	}
	if len(anchored.ExternalAnchors) == 0 || anchored.ExternalAnchors[0].Ref != declaration.FactIDs[0] {
		t.Errorf("anchored external anchors = %v, want the declaration parent", anchored.ExternalAnchors)
	}

	visibleOnly, err := store.ReadCausalClosure(context.Background(), lawReader, capture.FactIDs, ModeCapturesOnly, "visible_only")
	if err != nil {
		t.Fatalf("ReadCausalClosure(visible_only): %v", err)
	}
	if len(visibleOnly.FactIDs()) != len(capture.FactIDs) {
		t.Errorf("visible_only closure = %v, want only the capture", visibleOnly.FactIDs())
	}
	if len(visibleOnly.ExternalAnchors) != 0 {
		t.Errorf("visible_only produced %d external anchors, want 0", len(visibleOnly.ExternalAnchors))
	}

	_, err = store.ReadCausalClosure(context.Background(), lawReader, capture.FactIDs, ModeCapturesOnly, "exact_set")
	wantErrorContaining(t, err, "closure policy")
}

// TestLawCausalClosureModeFilterDoesNotPruneTraversal ports
// test_causal_closure_mode_filter_does_not_prune_traversal: the mode filter
// selects what is *visible*, never how far the closure *walks*.
func TestLawCausalClosureModeFilterDoesNotPruneTraversal(t *testing.T) {
	store := newMemStore(t)
	ancestor := lawAppendDrafts(t, store, "law:closure-mode:ancestor", "owner:closure-mode",
		draft("ancestor", Capture, nil),
	)
	bridge := lawAppendDrafts(t, store, "law:closure-mode:bridge", "owner:closure-mode",
		draft("bridge", Declaration, nil, ancestor.FactIDs[0]),
	)
	root := lawAppendDrafts(t, store, "law:closure-mode:root", "owner:closure-mode",
		draft("root", Capture, nil, bridge.FactIDs[0]),
	)

	view, err := store.ReadCausalClosure(context.Background(), lawReader, root.FactIDs, ModeCapturesOnly, "visible_only")
	if err != nil {
		t.Fatalf("ReadCausalClosure: %v", err)
	}

	want := []string{ancestor.FactIDs[0], root.FactIDs[0]}
	got := view.FactIDs()
	if len(got) != len(want) {
		t.Fatalf("closure = %v, want %v (the declaration bridge must not prune the walk)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("closure[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(view.CausalEdges) != 0 {
		t.Errorf("causal edges = %v, want none across the hidden bridge", view.CausalEdges)
	}
	if len(view.ExternalAnchors) != 0 {
		t.Errorf("visible_only produced %d external anchors, want 0", len(view.ExternalAnchors))
	}
}

// TestLawFullInternalReadsRequireTrustedAuthority ports
// test_full_internal_reads_require_trusted_authority.
func TestLawFullInternalReadsRequireTrustedAuthority(t *testing.T) {
	store := newMemStore(t)
	receipt := lawAppendDrafts(t, store, "law:full-internal", "owner:internal", draft("step", Capture, nil))

	untrusted := ReadContext{ActorRef: "reader", VisibilityProfile: VisibilityFullInternal}
	trusted := ReadContext{
		ActorRef:             "reader",
		PresentedWitnessRefs: []string{"trusted:internal"},
		VisibilityProfile:    VisibilityFullInternal,
	}

	_, err := store.ReadOwnerPrefix(context.Background(), untrusted, "owner:internal", 99, ModeBoth)
	wantErrorContaining(t, err, "full_internal")

	view, err := store.ReadOwnerPrefix(context.Background(), trusted, "owner:internal", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(trusted): %v", err)
	}
	if len(view.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("trusted full_internal read = %v, want %v", view.FactIDs(), receipt.FactIDs)
	}
}

// TestLawCutSliceModeLaws ports the mode-filter half of
// test_cut_slice_mode_and_projection_laws. The projection half is not portable
// yet: ensure_projection_compatible and the execution schemas land with plan 02,
// so it is recorded as a dependency in docs/law-coverage.md rather than faked.
func TestLawCutSliceModeLaws(t *testing.T) {
	store := newMemStore(t)
	declaration := lawAppendDrafts(t, store, "law:cut-mode:declaration", "owner:cut-mode",
		draft("intent", Declaration, nil),
	)
	capture := lawAppendDrafts(t, store, "law:cut-mode:capture", "owner:cut-mode",
		draft("result", Capture, nil),
	)
	cut, err := store.PublishCut(context.Background(), lawAppend, FrontierSpec{
		FrontierID:         "law:cut:mode",
		TargetTraceOwnerID: "owner:cut-mode",
		ThroughFactID:      capture.FactIDs[len(capture.FactIDs)-1],
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	before, err := store.FactCount(context.Background())
	if err != nil {
		t.Fatalf("FactCount: %v", err)
	}

	both, err := store.ResolveCut(context.Background(), lawReader, cut.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut(both): %v", err)
	}
	declarations, err := store.ResolveCut(context.Background(), lawReader, cut.FrontierID, ModeDeclarationsOnly)
	if err != nil {
		t.Fatalf("ResolveCut(declarations_only): %v", err)
	}
	captures, err := store.ResolveCut(context.Background(), lawReader, cut.FrontierID, ModeCapturesOnly)
	if err != nil {
		t.Fatalf("ResolveCut(captures_only): %v", err)
	}

	if both.ModeFilter != ModeBoth {
		t.Errorf("slice mode_filter = %q, want %q", both.ModeFilter, ModeBoth)
	}
	if want := []string{declaration.FactIDs[0]}; len(declarations.FactIDs()) != 1 || declarations.FactIDs()[0] != want[0] {
		t.Errorf("declarations_only = %v, want %v", declarations.FactIDs(), want)
	}
	if want := []string{capture.FactIDs[0]}; len(captures.FactIDs()) != 1 || captures.FactIDs()[0] != want[0] {
		t.Errorf("captures_only = %v, want %v", captures.FactIDs(), want)
	}

	after, err := store.FactCount(context.Background())
	if err != nil {
		t.Fatalf("FactCount after: %v", err)
	}
	if after != before {
		t.Errorf("fact count moved %d → %d: a read must not append", before, after)
	}
}

// TestLawRestartPreservesRecordsCutsAndPaths ports
// test_restart_preserves_records_cuts_and_paths. The restart-idempotency half
// is separately pinned by the conformance suite; this law is about durability
// of what was already retained — records, published cuts and owner paths.
func TestLawRestartPreservesRecordsCutsAndPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.sqlite")

	store1, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	receipt := lawAppendDrafts(t, store1, "law:restart", "owner:restart", draft("step", Capture, nil))
	cut, err := store1.PublishCut(context.Background(), lawAppend, FrontierSpec{
		FrontierID:         "law:cut:restart",
		TargetTraceOwnerID: "owner:restart",
		ThroughFactID:      receipt.FactIDs[len(receipt.FactIDs)-1],
		PublisherOwnerID:   "owner:publisher",
	})
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	if err := store1.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer restarted.Close(context.Background())

	prefix, err := restarted.ReadOwnerPrefix(context.Background(), lawReader, "owner:restart", 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix after restart: %v", err)
	}
	if len(prefix.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("owner path after restart = %v, want %v", prefix.FactIDs(), receipt.FactIDs)
	}
	resolved, err := restarted.ResolveCut(context.Background(), lawReader, cut.FrontierID, ModeBoth)
	if err != nil {
		t.Fatalf("ResolveCut after restart: %v", err)
	}
	if len(resolved.FactIDs()) != len(receipt.FactIDs) {
		t.Errorf("cut after restart = %v, want %v", resolved.FactIDs(), receipt.FactIDs)
	}
}

// keysOf is a small helper for readable failure messages.
func keysOf(m map[string]VisibleRecord) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
