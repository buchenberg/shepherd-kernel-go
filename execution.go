package shepherd

import "context"

// Execution fact helpers and projection folds (plan 02 §1), mirroring
// shepherd2/schemas/execution.py. Schema refs, payload keys, modes and the ID
// derivation are cross-language contract surface: testdata/
// execution_vectors_v0.json pins them against the reference implementation.
//
// Derivation, verified against Python rather than guessed: execution_id_for is
// NOT canonical JSON — it is sha256 over the raw bytes "<intent>\0<local_ref>",
// hex, first 32 characters, with an "exec:" prefix. Same for relation ids with
// "rel:".

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	SchemaExecutionCreated   = "shepherd2.execution.created.v1"   // declaration
	SchemaExecutionStarted   = "shepherd2.execution.started.v1"   // capture
	SchemaExecutionCompleted = "shepherd2.execution.completed.v1" // capture
	SchemaExecutionFailed    = "shepherd2.execution.failed.v1"    // capture
)

// ExecutionProjection is the projection spec every ProjectExecution entry
// point enforces. Python's is mode_requirement="both".
var ExecutionProjection = ProjectionSpec{
	Name:            "shepherd2.execution.project",
	ModeRequirement: ProjectionRequiresBoth,
	RequiresPayload: true,
	AcceptsAnchors:  true,
}

// ExecutionSchemaLibrary describes the execution schema ring.
var ExecutionSchemaLibrary = &StaticSchemaLibrary{
	LibraryName: "shepherd2.execution",
	Refs: []string{
		SchemaExecutionCreated,
		SchemaExecutionStarted,
		SchemaExecutionCompleted,
		SchemaExecutionFailed,
	},
	Specs: []ProjectionSpec{ExecutionProjection},
}

// ExecutionStatus is the projected lifecycle state of an execution.
type ExecutionStatus string

const (
	ExecutionPending   ExecutionStatus = "pending"
	ExecutionRunning   ExecutionStatus = "running"
	ExecutionSucceeded ExecutionStatus = "succeeded"
	ExecutionFailed    ExecutionStatus = "failed"
)

// Execution is the projected execution value derived from retained trace
// facts. Python models parent_execution_id, error and the cutoff as
// Optionals; Go uses "" for the absent scalar fields, and Cutoff is always
// present — synthesized from the slice when no frontier backs it, exactly
// as Python's _cutoff_from_slice does.
type Execution struct {
	ExecutionID       string
	TaskRef           string
	ParentExecutionID string
	Status            ExecutionStatus
	Inputs            map[string]any
	Outputs           map[string]any
	Error             string
	StartedFactID     string
	TerminalFactID    string
	Cutoff            Frontier
}

// executionReadContext is the read context PublishExecutionFrontier uses to
// inspect the through-fact, mirroring Python's ReadContext(actor_ref=
// "execution:schema").
var executionReadContext = ReadContext{ActorRef: "execution:schema", VisibilityProfile: VisibilityPayload}

// ExecutionIDFor derives a stable execution id for an append-local execution
// reference: "exec:" + sha256("<appendIntentID>\0<localRef>")[:32].
//
// The plan sketch showed an error return; the derivation has no failure mode
// (it hashes two strings), so the Go signature drops it deliberately.
func ExecutionIDFor(appendIntentID, localRef string) string {
	sum := sha256.Sum256([]byte(appendIntentID + "\x00" + localRef))
	return "exec:" + hex.EncodeToString(sum[:])[:32]
}

// ExecutionCreatedDraft builds the execution entity fact (declaration). The
// created fact is the ID anchor: created carries task_ref, inputs and the
// parent link. An absent parent is JSON null, never "".
func ExecutionCreatedDraft(executionID, taskRef string, inputs map[string]any, parentExecutionID string) RecordDraft {
	return RecordDraft{
		Mode:      Declaration,
		SchemaRef: SchemaExecutionCreated,
		KindLabel: "execution_created",
		Payload: map[string]any{
			"execution_id":        executionID,
			"task_ref":            taskRef,
			"inputs":              nonNilMap(inputs),
			"parent_execution_id": nullableString(parentExecutionID),
		},
	}
}

// ExecutionStartedDraft builds the execution-started lifecycle fact (capture).
func ExecutionStartedDraft(executionID string) RecordDraft {
	return RecordDraft{
		Mode:      Capture,
		SchemaRef: SchemaExecutionStarted,
		KindLabel: "execution_started",
		Payload:   map[string]any{"execution_id": executionID},
	}
}

// ExecutionCompletedDraft builds the terminal-success lifecycle fact.
func ExecutionCompletedDraft(executionID string, outputs map[string]any) RecordDraft {
	return RecordDraft{
		Mode:      Capture,
		SchemaRef: SchemaExecutionCompleted,
		KindLabel: "execution_completed",
		Payload: map[string]any{
			"execution_id": executionID,
			"outputs":      nonNilMap(outputs),
		},
	}
}

// ExecutionFailedDraft builds the terminal-failure lifecycle fact.
func ExecutionFailedDraft(executionID, errMsg string) RecordDraft {
	return RecordDraft{
		Mode:      Capture,
		SchemaRef: SchemaExecutionFailed,
		KindLabel: "execution_failed",
		Payload: map[string]any{
			"execution_id": executionID,
			"error":        errMsg,
		},
	}
}

// CreateExecutionBatch builds the canonical creation/start append for one
// execution: a single group on the execution's own owner path, drafts in
// created-then-started order.
func CreateExecutionBatch(appendIntentID, executionID, taskRef string, inputs map[string]any, parentExecutionID string, causedBy []string) AppendBatch {
	return AppendBatch{
		AppendIntentID: appendIntentID,
		Groups: []AppendGroup{{
			TraceOwnerID:  executionID,
			CausalParents: causedBy,
			FactDrafts: []RecordDraft{
				ExecutionCreatedDraft(executionID, taskRef, inputs, parentExecutionID),
				ExecutionStartedDraft(executionID),
			},
		}},
	}
}

// CompleteExecutionBatch builds the canonical terminal-success append.
func CompleteExecutionBatch(appendIntentID, executionID string, outputs map[string]any, causedBy []string) AppendBatch {
	return AppendBatch{
		AppendIntentID: appendIntentID,
		Groups: []AppendGroup{{
			TraceOwnerID:  executionID,
			CausalParents: causedBy,
			FactDrafts:    []RecordDraft{ExecutionCompletedDraft(executionID, outputs)},
		}},
	}
}

// FailExecutionBatch builds the canonical terminal-failure append.
func FailExecutionBatch(appendIntentID, executionID, errMsg string, causedBy []string) AppendBatch {
	return AppendBatch{
		AppendIntentID: appendIntentID,
		Groups: []AppendGroup{{
			TraceOwnerID:  executionID,
			CausalParents: causedBy,
			FactDrafts:    []RecordDraft{ExecutionFailedDraft(executionID, errMsg)},
		}},
	}
}

// PublishExecutionFrontier publishes an execution-schema terminal frontier
// over the Ring 0 frontier ABI. The terminal-frontier law is enforced here:
// the through-fact must be payload-visible and must carry a completed or
// failed schema; anything else fails before the frontier exists.
func PublishExecutionFrontier(ctx context.Context, store *SQLiteTraceStore, auth AppendContext, frontierID, targetExecutionID, throughFactID, publisherExecutionID, appendIntentID string, causedBy []string) (Frontier, error) {
	visible, err := store.ReadFact(ctx, executionReadContext, throughFactID)
	if err != nil {
		return Frontier{}, fmt.Errorf("read through fact: %w", err)
	}
	through, ok := visible.(Record)
	if !ok {
		return Frontier{}, fmt.Errorf("PublishExecutionFrontier requires payload-visible facts")
	}
	if through.Envelope.SchemaRef != SchemaExecutionCompleted && through.Envelope.SchemaRef != SchemaExecutionFailed {
		return Frontier{}, fmt.Errorf("terminal execution frontier must target a terminal lifecycle fact, got %s", through.Envelope.SchemaRef)
	}
	return store.PublishFrontier(ctx, auth, FrontierSpec{
		FrontierID:         frontierID,
		TargetTraceOwnerID: targetExecutionID,
		ThroughFactID:      throughFactID,
		PublisherOwnerID:   publisherExecutionID,
		AppendIntentID:     appendIntentID,
		CausedBy:           causedBy,
	})
}

// ProjectExecution projects an execution from a payload-visible trace slice,
// folding the owner path in order: created → started → terminal. Facts whose
// payload execution_id is absent or the target are folded; anything else on
// the path is skipped. The last terminal fact wins, matching Python.
func ProjectExecution(traceSlice Slice, targetTraceOwnerID string, cutoff *Frontier) (*Execution, error) {
	if err := EnsureProjectionCompatible(traceSlice, ExecutionProjection); err != nil {
		return nil, err
	}

	exec := &Execution{
		ExecutionID: targetTraceOwnerID,
		Status:      ExecutionPending,
		Inputs:      map[string]any{},
		Outputs:     map[string]any{},
	}

	for _, factID := range traceSlice.OwnerPaths[targetTraceOwnerID] {
		visible, ok := traceSlice.FactsByID[factID]
		if !ok {
			return nil, fmt.Errorf("ProjectExecution: fact %s on the owner path is not in the slice", factID)
		}
		fact, isRecord := visible.(Record)
		if !isRecord {
			return nil, fmt.Errorf("ProjectExecution requires payload-visible facts")
		}
		if id, present := fact.Body.Payload["execution_id"]; present && id != nil {
			if s, isStr := id.(string); !isStr || s != targetTraceOwnerID {
				continue
			}
		}
		switch fact.Envelope.SchemaRef {
		case SchemaExecutionCreated:
			exec.TaskRef = payloadString(fact.Body.Payload, "task_ref")
			exec.ParentExecutionID = payloadString(fact.Body.Payload, "parent_execution_id")
			exec.Inputs = payloadMap(fact.Body.Payload, "inputs")
			exec.Status = ExecutionPending
		case SchemaExecutionStarted:
			exec.StartedFactID = fact.Envelope.RecordID
			exec.Status = ExecutionRunning
		case SchemaExecutionCompleted:
			exec.Outputs = payloadMap(fact.Body.Payload, "outputs")
			exec.Error = ""
			exec.TerminalFactID = fact.Envelope.RecordID
			exec.Status = ExecutionSucceeded
		case SchemaExecutionFailed:
			exec.Outputs = map[string]any{}
			exec.Error = payloadString(fact.Body.Payload, "error")
			exec.TerminalFactID = fact.Envelope.RecordID
			exec.Status = ExecutionFailed
		}
	}

	exec.Cutoff = executionCutoffFrom(traceSlice, targetTraceOwnerID)
	if cutoff != nil {
		exec.Cutoff = *cutoff
	}
	return exec, nil
}

// ProjectExecutionFromStore resolves a frontier and projects an execution.
func ProjectExecutionFromStore(ctx context.Context, store *SQLiteTraceStore, readContext ReadContext, cutoff Frontier) (*Execution, error) {
	slice, err := store.ResolveFrontier(ctx, readContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

// executionCutoffFrom synthesizes a cutoff for a slice, mirroring Python's
// _cutoff_from_slice: a frontier-backed slice yields the frontier's own
// values; otherwise the through fact is the owner path's last fact, with its
// ordinal when visible.
func executionCutoffFrom(traceSlice Slice, targetTraceOwnerID string) Frontier {
	if traceSlice.Frontier != nil {
		return *traceSlice.Frontier
	}
	ids := traceSlice.OwnerPaths[targetTraceOwnerID]
	if len(ids) == 0 {
		return Frontier{TargetTraceOwnerID: targetTraceOwnerID, ThroughOwnerOrdinal: -1}
	}
	through := ids[len(ids)-1]
	ordinal := -1
	if visible, ok := traceSlice.FactsByID[through]; ok {
		if fact, isRecord := visible.(Record); isRecord && fact.View != nil {
			ordinal = fact.View.OwnerOrdinal
		}
	}
	return Frontier{
		TargetTraceOwnerID:  targetTraceOwnerID,
		ThroughFactID:       through,
		ThroughOwnerOrdinal: ordinal,
	}
}

// payloadString reads a payload key as a string. Python coerces with str();
// the execution payloads always carry strings for these keys, so a missing or
// non-string value yields "" rather than a coerced surprise.
func payloadString(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	if s, isStr := v.(string); isStr {
		return s
	}
	return ""
}

// payloadMap reads a payload key as an object, yielding an empty map for
// anything else, as Python's isinstance dict check does.
func payloadMap(payload map[string]any, key string) map[string]any {
	v, ok := payload[key]
	if !ok {
		return map[string]any{}
	}
	if m, isMap := v.(map[string]any); isMap {
		return m
	}
	return map[string]any{}
}

// nonNilMap normalizes a nil map to an empty one: the canonical writer
// renders a nil map as JSON null and an empty map as {}, and the reference
// always carries dicts — so a nil here would silently change the record
// digest, and therefore the record id, for the same logical run.
func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
