package shepherd

// Effective-history projection (plan 02 §3), mirroring
// shepherd2/schemas/history.py: the "what did the run tree actually do"
// view — root execution, active children, parent-published facts, resolved
// from a frontier.

import "fmt"

const SchemaRuntimePublishedFact = "shepherd2.runtime.published_fact.v1"

// PublishedFact is a parent-published fact visible in effective history.
type PublishedFact struct {
	FactID string
	Kind   string
	Data   map[string]any
}

// EffectiveChild is one active child in a parent's effective history: the
// relation that links it plus its projected execution.
type EffectiveChild struct {
	Relation  ExecutionRelation
	Execution Execution
}

// EffectiveHistory is the projected effective history for one execution
// owner prefix. Relations carries every projected relation; Children only
// the active ones, matching Python.
type EffectiveHistory struct {
	Root           Execution
	Relations      []ExecutionRelation
	Children       []EffectiveChild
	PublishedFacts []PublishedFact
}

// ProjectEffectiveHistory projects the root execution, its active children
// and its parent-published facts from trace slices. Child slices are
// matched to relations by frontier id: a relation without a child frontier,
// or one whose frontier has no slice, is skipped.
func ProjectEffectiveHistory(rootSlice Slice, rootTraceOwnerID string, childSlices []Slice) (*EffectiveHistory, error) {
	root, err := ProjectExecution(rootSlice, rootTraceOwnerID, nil)
	if err != nil {
		return nil, err
	}
	relations, err := ProjectExecutionRelations(rootSlice, rootTraceOwnerID)
	if err != nil {
		return nil, err
	}

	childSlicesByFrontier := make(map[string]Slice)
	for _, slice := range childSlices {
		if slice.Frontier != nil {
			childSlicesByFrontier[slice.Frontier.FrontierID] = slice
		}
	}

	var children []EffectiveChild
	for _, relation := range activeRelations(relations) {
		if relation.ChildFrontierID == "" {
			continue
		}
		childSlice, ok := childSlicesByFrontier[relation.ChildFrontierID]
		if !ok {
			continue
		}
		// The slice was selected by frontier id but the projection keys on
		// the child execution id; a disagreement would fold an empty owner
		// path and fabricate a pending child, so it is a loud error. Adopt
		// validates this pairing at record time; this guards every other
		// path that could write a relation.
		if childSlice.Frontier == nil {
			return nil, fmt.Errorf("effective history: child slice for %s carries no frontier", relation.ChildFrontierID)
		}
		if childSlice.Frontier.TargetTraceOwnerID != relation.ChildExecutionID {
			return nil, fmt.Errorf("effective history: frontier %s targets %s, not the relation's child %s",
				relation.ChildFrontierID, childSlice.Frontier.TargetTraceOwnerID, relation.ChildExecutionID)
		}
		child, err := ProjectExecution(childSlice, relation.ChildExecutionID, nil)
		if err != nil {
			return nil, err
		}
		children = append(children, EffectiveChild{Relation: relation, Execution: *child})
	}

	published, err := projectPublishedFacts(rootSlice, rootTraceOwnerID)
	if err != nil {
		return nil, err
	}
	return &EffectiveHistory{
		Root:           *root,
		Relations:      relations,
		Children:       children,
		PublishedFacts: published,
	}, nil
}

// ProjectEffectiveHistoryFromStore resolves the root frontier and each
// active child's frontier, then projects.
func ProjectEffectiveHistoryFromStore(store *SQLiteTraceStore, readContext ReadContext, cutoff Frontier) (*EffectiveHistory, error) {
	rootSlice, err := store.ResolveFrontier(readContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	relations, err := ProjectExecutionRelations(rootSlice, cutoff.TargetTraceOwnerID)
	if err != nil {
		return nil, err
	}
	var childSlices []Slice
	for _, relation := range activeRelations(relations) {
		if relation.ChildFrontierID == "" {
			continue
		}
		slice, err := store.ResolveFrontier(readContext, relation.ChildFrontierID, ModeBoth)
		if err != nil {
			return nil, fmt.Errorf("resolve child frontier %s: %w", relation.ChildFrontierID, err)
		}
		childSlices = append(childSlices, slice)
	}
	return ProjectEffectiveHistory(rootSlice, cutoff.TargetTraceOwnerID, childSlices)
}

// activeRelations folds the relation history: the LATEST fact per relation
// id wins, and only spawned and adopted relations stay active. Abandoning a
// child therefore drops it from effective history, and adopting over a
// spawn replaces the spawn. First-seen order is preserved, as Python's
// insertion-ordered dict does.
func activeRelations(relations []ExecutionRelation) []ExecutionRelation {
	latest := make(map[string]ExecutionRelation)
	var order []string
	for _, relation := range relations {
		if _, seen := latest[relation.RelationID]; !seen {
			order = append(order, relation.RelationID)
		}
		latest[relation.RelationID] = relation
	}
	var active []ExecutionRelation
	for _, id := range order {
		relation := latest[id]
		if relation.RelationKind == RelationSpawned || relation.RelationKind == RelationAdopted {
			active = append(active, relation)
		}
	}
	return active
}

// projectPublishedFacts collects the parent-published facts from the owner
// path, in order.
func projectPublishedFacts(traceSlice Slice, traceOwnerID string) ([]PublishedFact, error) {
	var facts []PublishedFact
	for _, factID := range traceSlice.OwnerPaths[traceOwnerID] {
		visible, ok := traceSlice.FactsByID[factID]
		if !ok {
			return nil, fmt.Errorf("projectPublishedFacts: fact %s on the owner path is not in the slice", factID)
		}
		fact, isRecord := visible.(Record)
		if !isRecord {
			return nil, fmt.Errorf("ProjectEffectiveHistory requires payload-visible facts")
		}
		if fact.Envelope.SchemaRef != SchemaRuntimePublishedFact {
			continue
		}
		facts = append(facts, PublishedFact{
			FactID: fact.Envelope.RecordID,
			Kind:   payloadString(fact.Body.Payload, "kind"),
			Data:   payloadMap(fact.Body.Payload, "data"),
		})
	}
	return facts, nil
}
