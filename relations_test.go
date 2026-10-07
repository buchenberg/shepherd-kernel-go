package shepherd

// Execution relation tests (plan 02 §2), measured against the
// relation_sequences group in testdata/execution_vectors_v0.json.

import (
	"strings"
	"testing"
)

// TestRelationIDForMatchesPython pins the rel: derivation.
func TestRelationIDForMatchesPython(t *testing.T) {
	doc := loadExecutionVectors(t)
	for _, v := range doc.RelationIDs {
		if got := RelationIDFor(v.AppendIntentID, v.LocalRef); got != v.RelationID {
			t.Errorf("RelationIDFor(%q, %q) = %s, want %s", v.AppendIntentID, v.LocalRef, got, v.RelationID)
		}
	}
}

// TestRelationDraftShape pins the relation draft: a capture (not a
// declaration), kind relation_created, payload keys, and child_frontier_id
// as JSON null when absent.
func TestRelationDraftShape(t *testing.T) {
	draft := executionRelationCreatedDraft("rel:abc", RelationSpawned, "exec:parent", "exec:child", "")
	if draft.Mode != Capture || draft.SchemaRef != SchemaExecutionRelation || draft.KindLabel != "relation_created" {
		t.Errorf("relation draft: %+v", draft)
	}
	if draft.Payload["relation_id"] != "rel:abc" || draft.Payload["relation_kind"] != "spawned" {
		t.Errorf("relation payload: %v", draft.Payload)
	}
	if draft.Payload["parent_execution_id"] != "exec:parent" || draft.Payload["child_execution_id"] != "exec:child" {
		t.Errorf("relation payload endpoints: %v", draft.Payload)
	}
	if v := draft.Payload["child_frontier_id"]; v != nil {
		t.Errorf("absent child frontier must be JSON null, got %v", v)
	}

	withFrontier := executionRelationCreatedDraft("rel:abc", RelationAdopted, "exec:p", "exec:c", "frontier:x")
	if v := withFrontier.Payload["child_frontier_id"]; v != "frontier:x" {
		t.Errorf("child frontier = %v, want frontier:x", v)
	}
}

// TestRelationVectorSequenceReplay replays the Python relation fixture —
// parent execution, child execution, spawned relation — and must allocate
// identical ids for every fact, with the relation landing on the parent's
// owner path.
func TestRelationVectorSequenceReplay(t *testing.T) {
	doc := loadExecutionVectors(t)
	vec := doc.RelationSequence
	store := newMemStore(t)

	parentCreate, err := store.Append(TrustedAppendContext, createExecutionBatch(
		vec.ParentCreateIntent, vec.ParentExecutionID, vec.ParentTaskRef, map[string]any{}, "", nil,
	))
	if err != nil {
		t.Fatalf("parent create: %v", err)
	}
	if parentCreate.FactIDs[0] == "" {
		t.Fatal("no parent facts")
	}
	childCreate, err := store.Append(TrustedAppendContext, createExecutionBatch(
		vec.ChildCreateIntent, vec.ChildExecutionID, vec.ChildTaskRef, vec.ChildInputs,
		vec.ParentExecutionID, parentCreate.FactIDs[len(parentCreate.FactIDs)-1:],
	))
	if err != nil {
		t.Fatalf("child create: %v", err)
	}
	childComplete, err := store.Append(TrustedAppendContext, completeExecutionBatch(
		vec.ChildCompleteIntent, vec.ChildExecutionID, vec.ChildOutputs,
		childCreate.FactIDs[len(childCreate.FactIDs)-1:],
	))
	if err != nil {
		t.Fatalf("child complete: %v", err)
	}
	if _, err := publishExecutionFrontier(store, TrustedAppendContext,
		vec.ChildFrontierID, vec.ChildExecutionID, childComplete.FactIDs[len(childComplete.FactIDs)-1], "", "", nil); err != nil {
		t.Fatalf("child frontier: %v", err)
	}

	relation, err := store.Append(TrustedAppendContext, createExecutionRelationBatch(
		vec.RelationIntent, vec.RelationID, RelationSpawned,
		vec.ParentExecutionID, vec.ChildExecutionID, vec.ChildFrontierID,
		parentCreate.FactIDs[len(parentCreate.FactIDs)-1:],
	))
	if err != nil {
		t.Fatalf("relation append: %v", err)
	}
	if relation.FactIDs[0] != vec.RelationFactID {
		t.Errorf("relation fact = %s, want %s", relation.FactIDs[0], vec.RelationFactID)
	}

	// Relations are parent-owned: the fact must sit on the parent's owner
	// path, which is what projectEffectiveHistory relies on.
	slice, err := store.ReadOwnerPrefix(reader, vec.ParentExecutionID, 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix(parent): %v", err)
	}
	found := false
	for _, id := range slice.FactIDs() {
		if id == vec.RelationFactID {
			found = true
		}
	}
	if !found {
		t.Errorf("relation fact %s is not on the parent owner path %v", vec.RelationFactID, slice.FactIDs())
	}

	relations, err := projectExecutionRelations(slice, vec.ParentExecutionID)
	if err != nil {
		t.Fatalf("projectExecutionRelations: %v", err)
	}
	if len(relations) != 1 {
		t.Fatalf("projected %d relations, want 1", len(relations))
	}
	got := relations[0]
	if got.RelationID != vec.RelationID || got.RelationKind != RelationSpawned ||
		got.ParentExecutionID != vec.ParentExecutionID ||
		got.ChildExecutionID != vec.ChildExecutionID ||
		got.ChildFrontierID != vec.ChildFrontierID ||
		got.CreatedFactID != vec.RelationFactID {
		t.Errorf("projected relation = %+v, want rel %s spawned %s->%s via %s",
			got, vec.RelationID, vec.ParentExecutionID, vec.ChildExecutionID, vec.ChildFrontierID)
	}
}

// TestRelationFromFactRejectsForeignSchema pins the type check.
func TestRelationFromFactRejectsForeignSchema(t *testing.T) {
	fact := Record{Envelope: RecordEnvelope{SchemaRef: SchemaExecutionCreated}}
	if _, err := executionRelationFromFact(fact); err == nil {
		t.Fatal("relation projection accepted a created fact")
	}
}

// TestRelationUnknownKindRejected pins the vocabulary: anything but
// spawned/adopted/abandoned fails loudly.
func TestRelationUnknownKindRejected(t *testing.T) {
	_, err := relationFromPayload("sha256:x", map[string]any{
		"relation_id":         "rel:abc",
		"relation_kind":       "teleported",
		"parent_execution_id": "exec:p",
		"child_execution_id":  "exec:c",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown execution relation kind") {
		t.Errorf("unknown kind: got %v, want a loud failure", err)
	}
}
