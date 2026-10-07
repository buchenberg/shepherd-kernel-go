package shepherd

// Runtime handles (plan 02 §5), mirroring shepherd2/runtime/handles.py: a
// synchronous facade over the store. No scheduler, no goroutine management
// beyond the one StartTask spawns — callers wrap in goroutines if they want
// concurrency.
//
// Python is strictly synchronous; the Go addition is StartTask, which runs
// the body in a goroutine and hands back a live Run whose Wait blocks until
// the terminal frontier exists. StartTaskSync is the faithful port — it runs
// the body inline, exactly like Python's @task start, and its record trace
// is ID-identical to the reference for the same fixture (pinned by
// TestStartTaskSyncMatchesPythonVector).
//
// Error-shape divergence, recorded rather than hidden: Python formats a task
// failure as "{ExceptionType}: {message}" because exceptions carry types.
// Go's TaskFunc returns (map, error); a failed run records err.Error() as
// the failure text, and a panic records "panic: <value>". The record shapes
// are identical — only the error text's provenance differs.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// trustedHandlesReadContext mirrors Python's TRUSTED_READ_CONTEXT.
var trustedHandlesReadContext = ReadContext{
	ActorRef:             "runtime:internal",
	PresentedWitnessRefs: []string{"trusted:internal"},
	VisibilityProfile:    VisibilityPayload,
}

// TaskFunc is one programmatic task body. It returns the run's outputs; a
// nil map means no outputs. Python's _coerce_outputs has a {"result": x}
// fallback for non-dict returns — Go's signature makes that case
// unexpressible, so nil is the only empty form.
type TaskFunc func(control *TaskControl) (map[string]any, error)

// Registry maps task refs to bodies. The task ref is the caller's string —
// Python derives it from the class's module and qualname; Go has no
// equivalent, so the registry key IS the task ref, recorded in the created
// fact verbatim.
type Registry struct {
	funcs map[string]TaskFunc
}

func NewRegistry() *Registry {
	return &Registry{funcs: map[string]TaskFunc{}}
}

func (r *Registry) Register(taskRef string, fn TaskFunc) {
	r.funcs[taskRef] = fn
}

func (r *Registry) lookup(taskRef string) (TaskFunc, error) {
	fn, ok := r.funcs[taskRef]
	if !ok {
		return nil, fmt.Errorf("no task registered under ref %q", taskRef)
	}
	return fn, nil
}

// Run is the live handle for one root execution.
type Run struct {
	store       *SQLiteTraceStore
	executionID string
	frontierID  string
}

func (r *Run) ExecutionID() string { return r.executionID }
func (r *Run) FrontierID() string  { return r.frontierID }

// Wait returns the terminal projected execution. For a StartTask run the
// terminal frontier may not exist yet; Wait polls for it until the context
// is cancelled.
func (r *Run) Wait(ctx context.Context) (*Execution, error) {
	cutoff, err := waitCutoff(ctx, r.store, r.frontierID)
	if err != nil {
		return nil, err
	}
	slice, err := r.store.ResolveFrontier(trustedHandlesReadContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

// Snapshot returns the current projected execution. Python documents that
// its runtime executes synchronously so snapshot and wait resolve the same
// retained cutoff; the same holds for StartTaskSync, and for StartTask the
// distinction is not observable through the store until the frontier exists.
func (r *Run) Snapshot(ctx context.Context) (*Execution, error) {
	return r.Wait(ctx)
}

// Cutoff returns the durable terminal read receipt for this run.
func (r *Run) Cutoff(ctx context.Context) (Frontier, error) {
	return waitCutoff(ctx, r.store, r.frontierID)
}

// ChildHandle is the live handle for a child execution related to a parent.
type ChildHandle struct {
	store       *SQLiteTraceStore
	executionID string
	frontierID  string
	relationID  string
	relation    ExecutionRelation
}

func (h *ChildHandle) ExecutionID() string         { return h.executionID }
func (h *ChildHandle) FrontierID() string          { return h.frontierID }
func (h *ChildHandle) RelationID() string          { return h.relationID }
func (h *ChildHandle) Relation() ExecutionRelation { return h.relation }

func (h *ChildHandle) Wait(ctx context.Context) (*Execution, error) {
	cutoff, err := waitCutoff(ctx, h.store, h.frontierID)
	if err != nil {
		return nil, err
	}
	slice, err := h.store.ResolveFrontier(trustedHandlesReadContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

func (h *ChildHandle) Snapshot(ctx context.Context) (*Execution, error) {
	return h.Wait(ctx)
}

func (h *ChildHandle) Cutoff(ctx context.Context) (Frontier, error) {
	return waitCutoff(ctx, h.store, h.frontierID)
}

// waitCutoff reads a terminal cutoff, polling while it does not exist yet —
// the one piece of waiting Python never needs, because its runtime is
// synchronous and a Run only exists after the frontier is published.
func waitCutoff(ctx context.Context, store *SQLiteTraceStore, frontierID string) (Frontier, error) {
	for {
		cutoff, err := store.ReadOwnerCutoff(frontierID)
		if err == nil {
			return cutoff, nil
		}
		select {
		case <-ctx.Done():
			return Frontier{}, fmt.Errorf("waiting for frontier %s: %w", frontierID, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TaskControl is the parent-owned control surface available while a task
// body runs.
type TaskControl struct {
	store                *SQLiteTraceStore
	executionID          string
	runID                string
	causalTail           string
	childIndex           int
	relationIndex        int
	publishIndex         int
	frontiersByExecution map[string]string
}

func newTaskControl(store *SQLiteTraceStore, executionID, runID, causalTail string) *TaskControl {
	return &TaskControl{
		store:                store,
		executionID:          executionID,
		runID:                runID,
		causalTail:           causalTail,
		frontiersByExecution: map[string]string{},
	}
}

// CausalTail returns the latest parent-visible fact for subsequent appends.
func (c *TaskControl) CausalTail() string { return c.causalTail }

// Publish appends a parent-owned fact into the current execution trace and
// advances the causal tail to it. Python returns the Fact; Go returns the
// retained Record.
func (c *TaskControl) Publish(kind string, data map[string]any) (Record, error) {
	c.publishIndex++
	receipt, err := c.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:publish:%d", c.runID, c.publishIndex),
		Groups: []AppendGroup{{
			TraceOwnerID:  c.executionID,
			CausalParents: []string{c.causalTail},
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaRuntimePublishedFact,
				KindLabel: "fact_published",
				Payload:   map[string]any{"kind": kind, "data": data},
			}},
		}},
	})
	if err != nil {
		return Record{}, err
	}
	c.causalTail = receipt.FactIDs[len(receipt.FactIDs)-1]
	visible, err := c.store.ReadFact(trustedHandlesReadContext, c.causalTail)
	if err != nil {
		return Record{}, err
	}
	fact, ok := visible.(Record)
	if !ok {
		return Record{}, fmt.Errorf("runtime publish expected payload-visible fact")
	}
	return fact, nil
}

// Spawn runs a child task synchronously and returns its handle, recording a
// spawned relation on this execution's owner path. The child's run id is
// "<parent run id>:child:<n>".
func (c *TaskControl) Spawn(taskRef string, inputs map[string]any, reg *Registry) (*ChildHandle, error) {
	c.childIndex++
	childRunID := fmt.Sprintf("%s:child:%d", c.runID, c.childIndex)
	childCreateIntent := childRunID + ":create"
	childExecutionID := ExecutionIDFor(childCreateIntent, "execution")
	childFrontierID := "frontier:" + childRunID + ":terminal"
	relationIntent := childRunID + ":relation:spawned"
	relationID := RelationIDFor(relationIntent, "relation")

	relationReceipt, err := c.store.Append(TrustedAppendContext, CreateExecutionRelationBatch(
		relationIntent, relationID, RelationSpawned,
		c.executionID, childExecutionID, childFrontierID,
		[]string{c.causalTail},
	))
	if err != nil {
		return nil, err
	}
	relation, err := c.readRelation(relationReceipt.FactIDs[0])
	if err != nil {
		return nil, err
	}
	c.causalTail = relation.CreatedFactID

	childRun, err := runTaskSync(c.store, reg, taskRef, childRunID, inputs,
		c.executionID, []string{relation.CreatedFactID},
		c.executionID, []string{relation.CreatedFactID})
	if err != nil {
		return nil, err
	}
	c.frontiersByExecution[childRun.executionID] = childRun.frontierID
	c.advanceToTerminalFact(childRun.frontierID)
	return &ChildHandle{
		store:       c.store,
		executionID: childRun.executionID,
		frontierID:  childRun.frontierID,
		relationID:  relationID,
		relation:    relation,
	}, nil
}

// AwaitTerminal observes a child's terminal cutoff and returns the projected
// execution. It also advances this control's causal tail to the child's
// terminal frontier record, matching the reference.
func (c *TaskControl) AwaitTerminal(ctx context.Context, handle *ChildHandle) (*Execution, error) {
	c.frontiersByExecution[handle.executionID] = handle.frontierID
	cutoff, err := handle.Cutoff(ctx)
	if err != nil {
		return nil, err
	}
	c.causalTail = cutoff.CreatedByFactID
	if c.causalTail == "" {
		c.causalTail = cutoff.ThroughFactID
	}
	return handle.Wait(ctx)
}

// ReadExecution reads a known child execution from its retained frontier.
func (c *TaskControl) ReadExecution(executionID string) (*Execution, error) {
	frontierID, ok := c.frontiersByExecution[executionID]
	if !ok {
		return nil, fmt.Errorf("execution is not known to this control: %s", executionID)
	}
	cutoff, err := c.store.ReadOwnerCutoff(frontierID)
	if err != nil {
		return nil, err
	}
	slice, err := c.store.ResolveFrontier(trustedHandlesReadContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

// Adopt records a parent-owned adopted relation to an existing execution.
// An empty relationID derives one from the adopt intent.
func (c *TaskControl) Adopt(executionID, frontierID, relationID string) (*ChildHandle, error) {
	c.relationIndex++
	relationIntent := fmt.Sprintf("%s:relation:adopted:%d", c.runID, c.relationIndex)
	if relationID == "" {
		relationID = RelationIDFor(relationIntent, "relation")
	}
	relationReceipt, err := c.store.Append(TrustedAppendContext, CreateExecutionRelationBatch(
		relationIntent, relationID, RelationAdopted,
		c.executionID, executionID, frontierID,
		[]string{c.causalTail},
	))
	if err != nil {
		return nil, err
	}
	relation, err := c.readRelation(relationReceipt.FactIDs[0])
	if err != nil {
		return nil, err
	}
	c.frontiersByExecution[executionID] = frontierID
	c.causalTail = relation.CreatedFactID
	return &ChildHandle{
		store:       c.store,
		executionID: executionID,
		frontierID:  frontierID,
		relationID:  relationID,
		relation:    relation,
	}, nil
}

// Abandon records that a related child is no longer effective for this
// parent, keyed by the child handle's relation id so the latest-wins fold
// drops it from effective history.
func (c *TaskControl) Abandon(handle *ChildHandle) (ExecutionRelation, error) {
	c.relationIndex++
	cutoff, err := handle.Cutoff(context.Background())
	if err != nil {
		return ExecutionRelation{}, err
	}
	// Python dedupes (causal_tail, created_by or through) preserving order;
	// the pair is at most two entries.
	causedBy := []string{c.causalTail}
	terminal := cutoff.CreatedByFactID
	if terminal == "" {
		terminal = cutoff.ThroughFactID
	}
	if terminal != causedBy[0] {
		causedBy = append(causedBy, terminal)
	}
	relationReceipt, err := c.store.Append(TrustedAppendContext, CreateExecutionRelationBatch(
		fmt.Sprintf("%s:relation:abandoned:%d", c.runID, c.relationIndex),
		handle.relationID, RelationAbandoned,
		c.executionID, handle.executionID, handle.frontierID,
		causedBy,
	))
	if err != nil {
		return ExecutionRelation{}, err
	}
	relation, err := c.readRelation(relationReceipt.FactIDs[0])
	if err != nil {
		return ExecutionRelation{}, err
	}
	c.causalTail = relation.CreatedFactID
	return relation, nil
}

func (c *TaskControl) readRelation(factID string) (ExecutionRelation, error) {
	visible, err := c.store.ReadFact(trustedHandlesReadContext, factID)
	if err != nil {
		return ExecutionRelation{}, err
	}
	fact, ok := visible.(Record)
	if !ok {
		return ExecutionRelation{}, fmt.Errorf("runtime relation projection expected payload-visible fact")
	}
	return ExecutionRelationFromFact(fact)
}

// advanceToTerminalFact moves the causal tail to the frontier record a
// terminal cutoff names, so subsequent appends chain after the child's
// completion.
func (c *TaskControl) advanceToTerminalFact(frontierID string) {
	cutoff, err := c.store.ReadOwnerCutoff(frontierID)
	if err != nil {
		return
	}
	c.causalTail = cutoff.CreatedByFactID
	if c.causalTail == "" {
		c.causalTail = cutoff.ThroughFactID
	}
}

// StartTaskSync runs a task synchronously — the faithful port of Python's
// @task start. An empty runID generates one ("run:<32 hex>", the shape of
// Python's uuid4).hex). The returned Run is terminal by the time it exists.
func StartTaskSync(store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any) (*Run, error) {
	if runID == "" {
		var err error
		runID, err = generateRunID()
		if err != nil {
			return nil, err
		}
	}
	return runTaskSync(store, reg, taskRef, runID, inputs, "", nil, "", nil)
}

// StartTask runs a task in a goroutine and returns immediately with a live
// Run; Wait blocks until the body has recorded its terminal frontier. This
// is the Go-idiom addition documented in the file header.
func StartTask(ctx context.Context, store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any) (*Run, error) {
	if runID == "" {
		var err error
		runID, err = generateRunID()
		if err != nil {
			return nil, err
		}
	}
	// The run's identity is fully derived from the run id, so the handle can
	// exist before the body finishes.
	run := &Run{store: store, executionID: ExecutionIDFor(runID+":create", "execution"), frontierID: "frontier:" + runID + ":terminal"}
	go func() {
		_, _ = runTaskSync(store, reg, taskRef, runID, inputs, "", nil, "", nil)
	}()
	return run, nil
}

func generateRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return "run:" + hex.EncodeToString(b[:]), nil
}

// runTaskSync is the faithful port of Python's _run_task_sync: idempotent by
// terminal-frontier existence, create → body → complete-or-fail → terminal
// frontier, with the causal wiring the vectors record.
func runTaskSync(store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any,
	parentExecutionID string, createCausedBy []string,
	frontierPublisherExecutionID string, frontierCausedBy []string) (*Run, error) {
	createIntent := runID + ":create"
	completeIntent := runID + ":complete"
	failIntent := runID + ":fail"
	frontierID := "frontier:" + runID + ":terminal"
	executionID := ExecutionIDFor(createIntent, "execution")

	// Idempotency: a rerun of the same run id against a durable store returns
	// the retained run without appending.
	if _, err := store.ReadOwnerCutoff(frontierID); err == nil {
		return &Run{store: store, executionID: executionID, frontierID: frontierID}, nil
	}

	createReceipt, err := store.Append(TrustedAppendContext, CreateExecutionBatch(
		createIntent, executionID, taskRef, inputs, parentExecutionID, createCausedBy,
	))
	if err != nil {
		return nil, err
	}
	control := newTaskControl(store, executionID, runID, createReceipt.FactIDs[len(createReceipt.FactIDs)-1])

	fn, err := reg.lookup(taskRef)
	if err != nil {
		// Python raises before the try block, so a missing task never reaches
		// the fail path; mirror that — the create is already retained.
		return nil, err
	}

	var terminalReceipt AppendReceipt
	outputs, runErr := runTaskBody(fn, control)
	if runErr == nil {
		terminalReceipt, err = store.Append(TrustedAppendContext, CompleteExecutionBatch(
			completeIntent, executionID, outputs, []string{control.causalTail},
		))
	} else {
		terminalReceipt, err = store.Append(TrustedAppendContext, FailExecutionBatch(
			failIntent, executionID, runErr.Error(), []string{control.causalTail},
		))
	}
	if err != nil {
		return nil, err
	}

	if _, err := PublishExecutionFrontier(store, TrustedAppendContext,
		frontierID, executionID, terminalReceipt.FactIDs[len(terminalReceipt.FactIDs)-1],
		frontierPublisherExecutionID, "", frontierCausedBy); err != nil {
		return nil, err
	}
	return &Run{store: store, executionID: executionID, frontierID: frontierID}, nil
}

// runTaskBody runs one task body, converting a panic into the error the fail
// path records — the Go equivalent of Python's `except Exception`.
func runTaskBody(fn TaskFunc, control *TaskControl) (outputs map[string]any, runErr error) {
	defer func() {
		if r := recover(); r != nil {
			outputs = nil
			runErr = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(control)
}
