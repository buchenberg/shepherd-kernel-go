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
	"errors"
	"fmt"
	"sync"
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
//
// The map is mutex-guarded because StartTask runs lookups on task
// goroutines; registering while a task is in flight would otherwise be a
// fatal concurrent map access.
type Registry struct {
	mu    sync.RWMutex
	funcs map[string]TaskFunc
}

func NewRegistry() *Registry {
	return &Registry{funcs: map[string]TaskFunc{}}
}

func (r *Registry) Register(taskRef string, fn TaskFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[taskRef] = fn
}

func (r *Registry) lookup(taskRef string) (TaskFunc, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.funcs[taskRef]
	if !ok {
		return nil, fmt.Errorf("no task registered under ref %q", taskRef)
	}
	return fn, nil
}

// Run is the live handle for one root execution. For a StartTask run it
// carries the body's completion channel, so a failed body surfaces through
// Wait instead of polling forever for a frontier that will never exist.
type Run struct {
	store       *SQLiteTraceStore
	executionID string
	frontierID  string
	done        <-chan error // nil for StartTaskSync runs
}

func (r *Run) ExecutionID() string { return r.executionID }
func (r *Run) FrontierID() string  { return r.frontierID }

// Wait returns the terminal projected execution. For a StartTask run the
// terminal frontier may not exist yet: the not-published condition is
// retried, real store errors return immediately, and a failed body returns
// its error rather than a hang.
func (r *Run) Wait(ctx context.Context) (*Execution, error) {
	cutoff, err := r.waitForCutoff(ctx)
	if err != nil {
		return nil, err
	}
	slice, err := r.store.ResolveFrontier(ctx, trustedHandlesReadContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

// Snapshot returns the CURRENT projected execution — pending, running or
// terminal — without waiting for anything: it folds the owner prefix as it
// stands. Unlike Wait it never blocks on the frontier.
func (r *Run) Snapshot(ctx context.Context) (*Execution, error) {
	// ctx threads into the owner-prefix read like every other store call.
	slice, err := r.store.ReadOwnerPrefix(ctx, trustedHandlesReadContext, r.executionID, 99, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, r.executionID, nil)
}

// Cutoff returns the durable terminal read receipt for this run.
func (r *Run) Cutoff(ctx context.Context) (Frontier, error) {
	return r.waitForCutoff(ctx)
}

// waitForCutoff polls only for the not-published condition
// (ErrUnknownFrontier); every other error — a closed store, a corrupt
// frontier, a resolver disagreement — returns immediately instead of
// spinning as "not yet". A failed body that left no frontier surfaces
// through the done channel.
func (r *Run) waitForCutoff(ctx context.Context) (Frontier, error) {
	for {
		cutoff, err := r.store.ReadOwnerCutoff(ctx, r.frontierID)
		if err == nil {
			return cutoff, nil
		}
		if !errors.Is(err, ErrUnknownFrontier) {
			return Frontier{}, err
		}
		select {
		case runErr, ok := <-r.done:
			if ok && runErr != nil {
				return Frontier{}, fmt.Errorf("task run failed: %w", runErr)
			}
		case <-ctx.Done():
			return Frontier{}, fmt.Errorf("waiting for frontier %s: %w", r.frontierID, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ChildHandle is the live handle for a child execution related to a parent.
// A child handle only exists once the child is terminal — Spawn runs the
// child synchronously, and Adopt validates the frontier before recording
// the relation — so Wait never waits: it reads the retained cutoff once,
// exactly like Python's handle, whose cutoff property would raise on a
// missing frontier.
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
	cutoff, err := h.store.ReadOwnerCutoff(ctx, h.frontierID)
	if err != nil {
		return nil, err
	}
	slice, err := h.store.ResolveFrontier(ctx, trustedHandlesReadContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

// Snapshot returns the child's current projection from its retained
// frontier. A spawned child is terminal by the time its handle exists.
func (h *ChildHandle) Snapshot(ctx context.Context) (*Execution, error) {
	return h.Wait(ctx)
}

func (h *ChildHandle) Cutoff(ctx context.Context) (Frontier, error) {
	return h.store.ReadOwnerCutoff(ctx, h.frontierID)
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
// retained Record. A nil data map publishes {}, never JSON null — Python's
// dict(data or {}) normalizes the same way.
func (c *TaskControl) Publish(ctx context.Context, kind string, data map[string]any) (Record, error) {
	c.publishIndex++
	receipt, err := c.store.Append(ctx, TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:publish:%d", c.runID, c.publishIndex),
		Groups: []AppendGroup{{
			TraceOwnerID:  c.executionID,
			CausalParents: []string{c.causalTail},
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaRuntimePublishedFact,
				KindLabel: "fact_published",
				Payload:   map[string]any{"kind": kind, "data": nonNilMap(data)},
			}},
		}},
	})
	if err != nil {
		return Record{}, err
	}
	c.causalTail = receipt.FactIDs[len(receipt.FactIDs)-1]
	visible, err := c.store.ReadFact(ctx, trustedHandlesReadContext, c.causalTail)
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
//
// The relation is recorded before the child runs — the cross-language
// contract, since the child's create batch is caused-by the relation fact.
// A child that cannot run records its own failure and still publishes a
// terminal frontier, so the relation's child_frontier_id resolves unless
// the store itself failed mid-spawn.
func (c *TaskControl) Spawn(ctx context.Context, taskRef string, inputs map[string]any, reg *Registry) (*ChildHandle, error) {
	c.childIndex++
	childRunID := fmt.Sprintf("%s:child:%d", c.runID, c.childIndex)
	childCreateIntent := childRunID + ":create"
	childExecutionID := ExecutionIDFor(childCreateIntent, "execution")
	childFrontierID := "frontier:" + childRunID + ":terminal"
	relationIntent := childRunID + ":relation:spawned"
	relationID := RelationIDFor(relationIntent, "relation")

	relationReceipt, err := c.store.Append(ctx, TrustedAppendContext, CreateExecutionRelationBatch(
		relationIntent, relationID, RelationSpawned,
		c.executionID, childExecutionID, childFrontierID,
		[]string{c.causalTail},
	))
	if err != nil {
		return nil, err
	}
	relation, err := c.readRelation(ctx, relationReceipt.FactIDs[0])
	if err != nil {
		return nil, err
	}
	c.causalTail = relation.CreatedFactID

	childRun, err := runTaskSync(ctx, c.store, reg, taskRef, childRunID, inputs,
		c.executionID, []string{relation.CreatedFactID},
		c.executionID, []string{relation.CreatedFactID})
	if err != nil {
		return nil, err
	}
	c.frontiersByExecution[childRun.executionID] = childRun.frontierID
	c.advanceToTerminalFact(ctx, childRun.frontierID)
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
	_ = ctx // kept for the plan's API; children are terminal already
	c.frontiersByExecution[handle.executionID] = handle.frontierID
	cutoff, err := handle.Cutoff(context.Background())
	if err != nil {
		return nil, err
	}
	c.causalTail = cutoff.CreatedByFactID
	if c.causalTail == "" {
		c.causalTail = cutoff.ThroughFactID
	}
	return handle.Wait(context.Background())
}

// ReadExecution reads a known child execution from its retained frontier.
func (c *TaskControl) ReadExecution(ctx context.Context, executionID string) (*Execution, error) {
	frontierID, ok := c.frontiersByExecution[executionID]
	if !ok {
		return nil, fmt.Errorf("execution is not known to this control: %s", executionID)
	}
	cutoff, err := c.store.ReadOwnerCutoff(ctx, frontierID)
	if err != nil {
		return nil, err
	}
	slice, err := c.store.ResolveFrontier(ctx, trustedHandlesReadContext, cutoff.FrontierID, ModeBoth)
	if err != nil {
		return nil, err
	}
	return ProjectExecution(slice, cutoff.TargetTraceOwnerID, &cutoff)
}

// Adopt records a parent-owned adopted relation to an existing execution.
// The frontier must exist and target the named execution — a dangling
// relation would make the parent's effective history unresolvable, so both
// are checked before anything is appended. An empty relationID derives one
// from the adopt intent.
func (c *TaskControl) Adopt(ctx context.Context, executionID, frontierID, relationID string) (*ChildHandle, error) {
	cutoff, err := c.store.ReadOwnerCutoff(ctx, frontierID)
	if err != nil {
		return nil, fmt.Errorf("adopt: frontier %s: %w", frontierID, err)
	}
	if cutoff.TargetTraceOwnerID != executionID {
		return nil, fmt.Errorf("adopt: frontier %s targets %s, not %s",
			frontierID, cutoff.TargetTraceOwnerID, executionID)
	}

	c.relationIndex++
	relationIntent := fmt.Sprintf("%s:relation:adopted:%d", c.runID, c.relationIndex)
	if relationID == "" {
		relationID = RelationIDFor(relationIntent, "relation")
	}
	relationReceipt, err := c.store.Append(ctx, TrustedAppendContext, CreateExecutionRelationBatch(
		relationIntent, relationID, RelationAdopted,
		c.executionID, executionID, frontierID,
		[]string{c.causalTail},
	))
	if err != nil {
		return nil, err
	}
	relation, err := c.readRelation(ctx, relationReceipt.FactIDs[0])
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
// drops it from effective history. The child's terminal cutoff is read once,
// without polling: Python's abandon reads handle.cutoff as a property, which
// raises on a missing frontier — a child without a terminal cutoff cannot
// be abandoned, it can only be left alone.
func (c *TaskControl) Abandon(ctx context.Context, handle *ChildHandle) (ExecutionRelation, error) {
	cutoff, err := c.store.ReadOwnerCutoff(ctx, handle.frontierID)
	if err != nil {
		return ExecutionRelation{}, fmt.Errorf("abandon: child has no terminal cutoff: %w", err)
	}
	c.relationIndex++
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
	relationReceipt, err := c.store.Append(ctx, TrustedAppendContext, CreateExecutionRelationBatch(
		fmt.Sprintf("%s:relation:abandoned:%d", c.runID, c.relationIndex),
		handle.relationID, RelationAbandoned,
		c.executionID, handle.executionID, handle.frontierID,
		causedBy,
	))
	if err != nil {
		return ExecutionRelation{}, err
	}
	relation, err := c.readRelation(ctx, relationReceipt.FactIDs[0])
	if err != nil {
		return ExecutionRelation{}, err
	}
	c.causalTail = relation.CreatedFactID
	return relation, nil
}

func (c *TaskControl) readRelation(ctx context.Context, factID string) (ExecutionRelation, error) {
	visible, err := c.store.ReadFact(ctx, trustedHandlesReadContext, factID)
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
func (c *TaskControl) advanceToTerminalFact(ctx context.Context, frontierID string) {
	cutoff, err := c.store.ReadOwnerCutoff(ctx, frontierID)
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
// Python's uuid4().hex). The returned Run is terminal by the time it exists.
func StartTaskSync(ctx context.Context, store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any) (*Run, error) {
	if runID == "" {
		var err error
		runID, err = generateRunID()
		if err != nil {
			return nil, err
		}
	}
	return runTaskSync(ctx, store, reg, taskRef, runID, inputs, "", nil, "", nil)
}

// StartTask runs a task in a goroutine and returns immediately with a live
// Run; Wait blocks until the body has recorded its terminal frontier, and a
// failed body surfaces through Wait as an error rather than a hang. This is
// the Go-idiom addition documented in the file header.
func StartTask(ctx context.Context, store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any) (*Run, error) {
	_ = ctx
	if runID == "" {
		var err error
		runID, err = generateRunID()
		if err != nil {
			return nil, err
		}
	}
	done := make(chan error, 1)
	run := &Run{
		store:       store,
		executionID: ExecutionIDFor(runID+":create", "execution"),
		frontierID:  "frontier:" + runID + ":terminal",
		done:        done,
	}
	go func() {
		done <- runTaskSyncError(ctx, store, reg, taskRef, runID, inputs, "", nil, "", nil)
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
func runTaskSync(ctx context.Context, store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any,
	parentExecutionID string, createCausedBy []string,
	frontierPublisherExecutionID string, frontierCausedBy []string) (*Run, error) {
	if err := runTaskSyncError(ctx, store, reg, taskRef, runID, inputs,
		parentExecutionID, createCausedBy, frontierPublisherExecutionID, frontierCausedBy); err != nil {
		return nil, err
	}
	return &Run{
		store:       store,
		executionID: ExecutionIDFor(runID+":create", "execution"),
		frontierID:  "frontier:" + runID + ":terminal",
	}, nil
}

func runTaskSyncError(ctx context.Context, store *SQLiteTraceStore, reg *Registry, taskRef, runID string, inputs map[string]any,
	parentExecutionID string, createCausedBy []string,
	frontierPublisherExecutionID string, frontierCausedBy []string) error {
	createIntent := runID + ":create"
	completeIntent := runID + ":complete"
	failIntent := runID + ":fail"
	frontierID := "frontier:" + runID + ":terminal"
	executionID := ExecutionIDFor(createIntent, "execution")

	// Idempotency: a rerun of the same run id against a store that already
	// holds the terminal frontier returns the retained run without appending
	// — and without re-invoking the body, whose external side effects are not
	// idempotent. Only the not-published condition counts as "run it"; any
	// other read error is real and returns.
	cutoff, err := store.ReadOwnerCutoff(ctx, frontierID)
	if err == nil {
		_ = cutoff
		return nil
	}
	if !errors.Is(err, ErrUnknownFrontier) {
		return err
	}

	createReceipt, err := store.Append(ctx, TrustedAppendContext, CreateExecutionBatch(
		createIntent, executionID, taskRef, inputs, parentExecutionID, createCausedBy,
	))
	if err != nil {
		return err
	}
	control := newTaskControl(store, executionID, runID, createReceipt.FactIDs[len(createReceipt.FactIDs)-1])

	// The body phase — lookup included — runs inside the failure boundary, as
	// Python's `try: instance = task_cls(**inputs)` does: a task that cannot
	// be constructed (or, here, resolved) records a failed execution rather
	// than leaving a permanently running one.
	outputs, runErr := runTaskBody(reg, taskRef, control)

	var terminalReceipt AppendReceipt
	if runErr == nil {
		terminalReceipt, err = store.Append(ctx, TrustedAppendContext, CompleteExecutionBatch(
			completeIntent, executionID, outputs, []string{control.causalTail},
		))
	} else {
		terminalReceipt, err = store.Append(ctx, TrustedAppendContext, FailExecutionBatch(
			failIntent, executionID, runErr.Error(), []string{control.causalTail},
		))
	}
	if err != nil {
		return err
	}

	_, err = PublishExecutionFrontier(ctx, store, TrustedAppendContext,
		frontierID, executionID, terminalReceipt.FactIDs[len(terminalReceipt.FactIDs)-1],
		frontierPublisherExecutionID, "", frontierCausedBy)
	return err
}

// runTaskBody runs one task body, resolving the task first: a lookup failure
// is a body failure and records through the fail path, and a panic is
// recovered into the error the fail path records — the Go equivalent of
// Python's `except Exception`.
func runTaskBody(reg *Registry, taskRef string, control *TaskControl) (outputs map[string]any, runErr error) {
	defer func() {
		if r := recover(); r != nil {
			outputs = nil
			runErr = fmt.Errorf("panic: %v", r)
		}
	}()
	fn, err := reg.lookup(taskRef)
	if err != nil {
		return nil, err
	}
	return fn(control)
}
