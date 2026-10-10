package shepherd

import "context"

// Parent-owned execution relation facts (plan 02 §2), mirroring
// shepherd2/schemas/relations.py.
//
// Relations live on the PARENT's owner path — that placement is what lets
// ProjectEffectiveHistory resolve a run tree from one frontier — and the
// draft is a capture, not a declaration: a relation records something that
// happened, not an intent.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const SchemaExecutionRelation = "shepherd2.execution_relation.created.v1"

// ExecutionRelationProjection is the spec ProjectExecutionRelations enforces.
var ExecutionRelationProjection = ProjectionSpec{
	Name:            "shepherd2.execution_relation.project",
	ModeRequirement: ProjectionRequiresBoth,
	RequiresPayload: true,
	AcceptsAnchors:  true,
}

// ExecutionRelationSchemaLibrary describes the relation schema ring.
var ExecutionRelationSchemaLibrary = &StaticSchemaLibrary{
	LibraryName: "shepherd2.execution_relation",
	Refs:        []string{SchemaExecutionRelation},
	Specs:       []ProjectionSpec{ExecutionRelationProjection},
}

// RelationKind describes what a parent did with a child execution.
type RelationKind string

const (
	RelationSpawned   RelationKind = "spawned"
	RelationAdopted   RelationKind = "adopted"
	RelationAbandoned RelationKind = "abandoned"
)

// ExecutionRelation is the projected parent-owned relation between two
// executions. ChildFrontierID is "" when the relation carries no child
// frontier (Python's None).
type ExecutionRelation struct {
	RelationID        string
	RelationKind      RelationKind
	ParentExecutionID string
	ChildExecutionID  string
	ChildFrontierID   string
	CreatedFactID     string
}

// RelationIDFor derives a stable relation id: "rel:" +
// sha256("<appendIntentID>\0<localRef>")[:32]. Same shape as
// ExecutionIDFor — pinned by testdata/execution_vectors_v0.json.
func RelationIDFor(appendIntentID, localRef string) string {
	sum := sha256.Sum256([]byte(appendIntentID + "\x00" + localRef))
	return "rel:" + hex.EncodeToString(sum[:])[:32]
}

// executionRelationCreatedDraft builds the parent-owned relation fact
// (capture). An absent child frontier is JSON null, never "".
func executionRelationCreatedDraft(relationID string, kind RelationKind, parentExecutionID, childExecutionID, childFrontierID string) RecordDraft {
	return RecordDraft{
		Mode:      Capture,
		SchemaRef: SchemaExecutionRelation,
		KindLabel: "relation_created",
		Payload: map[string]any{
			"relation_id":         relationID,
			"relation_kind":       string(kind),
			"parent_execution_id": parentExecutionID,
			"child_execution_id":  childExecutionID,
			"child_frontier_id":   nullableString(childFrontierID),
		},
	}
}

// CreateExecutionRelationBatch builds the canonical parent-owned relation
// append: the group's owner path is the PARENT's execution id.
func CreateExecutionRelationBatch(appendIntentID, relationID string, kind RelationKind, parentExecutionID, childExecutionID, childFrontierID string, causedBy []string) AppendBatch {
	return AppendBatch{
		AppendIntentID: appendIntentID,
		Groups: []AppendGroup{{
			TraceOwnerID:  parentExecutionID,
			CausalParents: causedBy,
			FactDrafts: []RecordDraft{
				executionRelationCreatedDraft(relationID, kind, parentExecutionID, childExecutionID, childFrontierID),
			},
		}},
	}
}

// ProjectExecutionRelations projects parent-owned relations from a
// payload-visible slice, in owner-path order. Facts carrying another
// parent's relation are skipped — relations are addressed by the owner path
// they live on.
func ProjectExecutionRelations(traceSlice Slice, parentTraceOwnerID string) ([]ExecutionRelation, error) {
	if err := EnsureProjectionCompatible(traceSlice, ExecutionRelationProjection); err != nil {
		return nil, err
	}
	var relations []ExecutionRelation
	for _, factID := range traceSlice.OwnerPaths[parentTraceOwnerID] {
		visible, ok := traceSlice.FactsByID[factID]
		if !ok {
			return nil, fmt.Errorf("ProjectExecutionRelations: fact %s on the owner path is not in the slice", factID)
		}
		fact, isRecord := visible.(Record)
		if !isRecord {
			return nil, fmt.Errorf("ProjectExecutionRelations requires payload-visible facts")
		}
		if fact.Envelope.SchemaRef != SchemaExecutionRelation {
			continue
		}
		relation, err := relationFromPayload(fact.Envelope.RecordID, fact.Body.Payload)
		if err != nil {
			return nil, err
		}
		if relation.ParentExecutionID != parentTraceOwnerID {
			continue
		}
		relations = append(relations, relation)
	}
	return relations, nil
}

// ProjectExecutionRelationsFromStore resolves a frontier and projects the
// parent-owned relations it covers.
func ProjectExecutionRelationsFromStore(ctx context.Context, store *SQLiteTraceStore, readContext ReadContext, cutoff Frontier) ([]ExecutionRelation, error) {
	slice, err := store.ResolveFrontier(ctx, readContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecutionRelations(slice, cutoff.TargetTraceOwnerID)
}

// ExecutionRelationFromFact projects one relation-created fact.
func ExecutionRelationFromFact(fact Record) (ExecutionRelation, error) {
	if fact.Envelope.SchemaRef != SchemaExecutionRelation {
		return ExecutionRelation{}, fmt.Errorf("expected relation_created fact, got %q", fact.Envelope.SchemaRef)
	}
	return relationFromPayload(fact.Envelope.RecordID, fact.Body.Payload)
}

// relationFromPayload rebuilds a relation from its retained payload. An
// unknown kind fails loudly: spawn/adopt/abandon is the whole vocabulary, so
// anything else means a payload the schema never produced. The identifier
// fields, by contrast, coerce missing or non-string values to "" — exactly
// as Python's str(payload.get(..., "")) does; a missing parent id therefore
// drops the relation in the projection on both sides of the port, which is
// parity, not an accident.
func relationFromPayload(factID string, payload map[string]any) (ExecutionRelation, error) {
	kind := payloadString(payload, "relation_kind")
	switch RelationKind(kind) {
	case RelationSpawned, RelationAdopted, RelationAbandoned:
	default:
		return ExecutionRelation{}, fmt.Errorf("unknown execution relation kind: %s", kind)
	}
	return ExecutionRelation{
		RelationID:        payloadString(payload, "relation_id"),
		RelationKind:      RelationKind(kind),
		ParentExecutionID: payloadString(payload, "parent_execution_id"),
		ChildExecutionID:  payloadString(payload, "child_execution_id"),
		ChildFrontierID:   payloadString(payload, "child_frontier_id"),
		CreatedFactID:     factID,
	}, nil
}
