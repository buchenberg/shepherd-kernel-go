package shepherd

// Effective-history tests (plan 02 §3). The fold semantics — latest relation
// per id wins, abandoned drops a child, adopted keeps it — are mirrored from
// schemas/history.py's _active_relations, which relies on owner-path order.

import "testing"

// historyFixture builds a parent with two published facts and three
// children: one spawned and completed (active), one spawned then abandoned
// (dropped), and one spawned then adopted (active as adopted). Returns the
// store and the parent's terminal frontier id.
func historyFixture(t *testing.T) (*SQLiteTraceStore, string) {
	t.Helper()
	store := newMemStore(t)
	parentID := ExecutionIDFor("hist:parent:create", "execution")

	parentCreate, err := store.Append(TrustedAppendContext, CreateExecutionBatch(
		"hist:parent:create", parentID, "ParentTask", nil, "", nil,
	))
	if err != nil {
		t.Fatalf("parent create: %v", err)
	}
	tail := parentCreate.FactIDs[len(parentCreate.FactIDs)-1]

	publish := func(intent, kind string) {
		t.Helper()
		receipt, err := store.Append(TrustedAppendContext, AppendBatch{
			AppendIntentID: intent,
			Groups: []AppendGroup{{
				TraceOwnerID:  parentID,
				CausalParents: []string{tail},
				FactDrafts: []RecordDraft{{
					Mode:      Capture,
					SchemaRef: SchemaRuntimePublishedFact,
					KindLabel: "fact_published",
					Payload:   map[string]any{"kind": kind, "data": map[string]any{"k": kind}},
				}},
			}},
		})
		if err != nil {
			t.Fatalf("publish %s: %v", kind, err)
		}
		tail = receipt.FactIDs[len(receipt.FactIDs)-1]
	}
	publish("hist:parent:publish:1", "note")
	publish("hist:parent:publish:2", "metric")

	child := func(runID, taskRef string) (string, string) {
		t.Helper()
		childID := ExecutionIDFor(runID+":create", "execution")
		create, err := store.Append(TrustedAppendContext, CreateExecutionBatch(
			runID+":create", childID, taskRef, nil, parentID, []string{tail},
		))
		if err != nil {
			t.Fatalf("child %s create: %v", runID, err)
		}
		complete, err := store.Append(TrustedAppendContext, CompleteExecutionBatch(
			runID+":complete", childID, map[string]any{"who": taskRef},
			create.FactIDs[len(create.FactIDs)-1:],
		))
		if err != nil {
			t.Fatalf("child %s complete: %v", runID, err)
		}
		frontierID := "frontier:" + runID + ":terminal"
		if _, err := PublishExecutionFrontier(store, TrustedAppendContext,
			frontierID, childID, complete.FactIDs[len(complete.FactIDs)-1], parentID, "", nil); err != nil {
			t.Fatalf("child %s frontier: %v", runID, err)
		}
		tail = complete.FactIDs[len(complete.FactIDs)-1]
		return childID, frontierID
	}

	spawnedID, spawnedFrontier := child("hist:parent:child:1", "SpawnedTask")
	abandonedID, abandonedFrontier := child("hist:parent:child:2", "AbandonedTask")
	adoptedID, adoptedFrontier := child("hist:parent:child:3", "AdoptedTask")

	relation := func(intent, relationID, kind, childID, frontierID string) {
		t.Helper()
		receipt, err := store.Append(TrustedAppendContext, CreateExecutionRelationBatch(
			intent, relationID, RelationKind(kind), parentID, childID, frontierID, []string{tail},
		))
		if err != nil {
			t.Fatalf("relation %s: %v", intent, err)
		}
		tail = receipt.FactIDs[len(receipt.FactIDs)-1]
	}

	// Spawned stays; spawned-then-abandoned drops; spawned-then-adopted
	// survives as adopted — all keyed by the same relation id, so the
	// latest-wins fold is what decides.
	relation("hist:parent:child:1:relation:spawned",
		RelationIDFor("hist:parent:child:1:relation:spawned", "relation"), "spawned",
		spawnedID, spawnedFrontier)
	relation("hist:parent:child:2:relation:spawned",
		RelationIDFor("hist:parent:child:2:relation:spawned", "relation"), "spawned",
		abandonedID, abandonedFrontier)
	relation("hist:parent:relation:abandoned:1",
		RelationIDFor("hist:parent:child:2:relation:spawned", "relation"), "abandoned",
		abandonedID, abandonedFrontier)
	relation("hist:parent:child:3:relation:spawned",
		RelationIDFor("hist:parent:child:3:relation:spawned", "relation"), "spawned",
		adoptedID, adoptedFrontier)
	relation("hist:parent:relation:adopted:1",
		RelationIDFor("hist:parent:child:3:relation:spawned", "relation"), "adopted",
		adoptedID, adoptedFrontier)

	// Terminate the parent through the last relation fact and publish its
	// frontier, so the whole tree is covered by one cutoff.
	complete, err := store.Append(TrustedAppendContext, CompleteExecutionBatch(
		"hist:parent:complete", parentID, map[string]any{"done": true}, []string{tail},
	))
	if err != nil {
		t.Fatalf("parent complete: %v", err)
	}
	frontierID := "frontier:hist:parent:terminal"
	if _, err := PublishExecutionFrontier(store, TrustedAppendContext,
		frontierID, parentID, complete.FactIDs[len(complete.FactIDs)-1], "", "", nil); err != nil {
		t.Fatalf("parent frontier: %v", err)
	}
	return store, frontierID
}

// TestProjectEffectiveHistory pins the fold: root execution, all relations
// projected, only the active children resolved, published facts in order.
func TestProjectEffectiveHistory(t *testing.T) {
	store, frontierID := historyFixture(t)
	cutoff, err := store.ReadOwnerCutoff(frontierID)
	if err != nil {
		t.Fatalf("ReadOwnerCutoff: %v", err)
	}

	history, err := ProjectEffectiveHistoryFromStore(store, reader, cutoff)
	if err != nil {
		t.Fatalf("ProjectEffectiveHistoryFromStore: %v", err)
	}

	if history.Root.ExecutionID != cutoff.TargetTraceOwnerID {
		t.Errorf("root = %s, want %s", history.Root.ExecutionID, cutoff.TargetTraceOwnerID)
	}
	if history.Root.Status != ExecutionSucceeded {
		t.Errorf("root status = %s, want succeeded", history.Root.Status)
	}

	// Every relation fact projects: 2 spawned + 1 abandoned + 1 adopted +
	// the second spawn of child 3 = 5 relations total.
	if len(history.Relations) != 5 {
		t.Errorf("projected %d relations, want 5: %+v", len(history.Relations), history.Relations)
	}

	// Only the spawned child and the adopted child stay active; the
	// abandoned child drops, and the adopted relation wins over its spawn.
	if len(history.Children) != 2 {
		t.Fatalf("effective children = %d, want 2", len(history.Children))
	}
	byTask := map[string]EffectiveChild{}
	for _, child := range history.Children {
		byTask[child.Execution.TaskRef] = child
	}
	spawned, ok := byTask["SpawnedTask"]
	if !ok {
		t.Fatal("spawned child missing from effective history")
	}
	if spawned.Relation.RelationKind != RelationSpawned {
		t.Errorf("spawned child relation kind = %s, want spawned", spawned.Relation.RelationKind)
	}
	adopted, ok := byTask["AdoptedTask"]
	if !ok {
		t.Fatal("adopted child missing from effective history")
	}
	if adopted.Relation.RelationKind != RelationAdopted {
		t.Errorf("adopted child relation kind = %s, want adopted (latest wins)", adopted.Relation.RelationKind)
	}
	if _, still := byTask["AbandonedTask"]; still {
		t.Error("abandoned child is still active in effective history")
	}
	if spawned.Execution.Status != ExecutionSucceeded || adopted.Execution.Status != ExecutionSucceeded {
		t.Error("children must project as succeeded")
	}

	if len(history.PublishedFacts) != 2 {
		t.Fatalf("published facts = %d, want 2", len(history.PublishedFacts))
	}
	if history.PublishedFacts[0].Kind != "note" || history.PublishedFacts[1].Kind != "metric" {
		t.Errorf("published kinds = %v, want [note metric]", history.PublishedFacts)
	}
}

// TestActiveRelationsLatestWins pins the fold rule in isolation, including
// first-seen ordering when a relation id is superseded.
func TestActiveRelationsLatestWins(t *testing.T) {
	rel := func(id string, kind RelationKind) ExecutionRelation {
		return ExecutionRelation{RelationID: id, RelationKind: kind}
	}
	relations := []ExecutionRelation{
		rel("rel:a", RelationSpawned),
		rel("rel:b", RelationSpawned),
		rel("rel:a", RelationAbandoned),
		rel("rel:c", RelationSpawned),
		rel("rel:b", RelationAdopted),
	}
	active := activeRelations(relations)
	// rel:a was abandoned (dropped); rel:b was adopted (kept, latest);
	// rel:c stays spawned. First-seen order: b before c.
	if len(active) != 2 {
		t.Fatalf("active = %d, want 2: %+v", len(active), active)
	}
	if active[0].RelationID != "rel:b" || active[0].RelationKind != RelationAdopted {
		t.Errorf("active[0] = %+v, want rel:b adopted", active[0])
	}
	if active[1].RelationID != "rel:c" || active[1].RelationKind != RelationSpawned {
		t.Errorf("active[1] = %+v, want rel:c spawned", active[1])
	}
}
