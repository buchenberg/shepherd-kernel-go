# Plan 02 — Schema Rings + Runtime Handles

**Phase**: 2 · **Priority**: P1 · **Estimate**: ~2–3 weeks (~1200–1800 LOC + ~1000 test LOC)
**Goal**: Port shepherd2 Ring 1 (schemas) and Ring 2 (runtime handles) so Go
traces become *interpretable*: executions with lifecycle projections, spawn/
adopt/abandon relations, effective-history folding, projection-purity checks,
and a synchronous task facade over the store.

**Depends on**: plan 01 §1 (canonical float/escape parity) — schema-derived IDs
are digests. Otherwise independent of plans 03–04.

**Python anchors** (under `../shepherd2/src/shepherd2/`):
- `schemas/execution.py` (315 L) · `schemas/relations.py` · `schemas/history.py`
- `schemas/schema_library.py` · `schemas/run_outputs.py` (**excluded** here —
  minimal subset lives in plan 04)
- `runtime/handles.py` (437 L)

**Go layout**: keep the flat root-package convention (`package shepherd`):
new files `execution.go`, `relations.go`, `history.go`, `schema_library.go`,
`handles.go` (+ matching `_test.go`). No new dependencies.

---

## 1. Execution lifecycle schema (execution.go)

Mirror `schemas/execution.py` exactly — schema refs, payload keys, modes, and
ID derivation are cross-language contract surface:

```go
const (
    SchemaExecutionCreated   = "shepherd2.execution.created.v1"   // Declaration
    SchemaExecutionStarted   = "shepherd2.execution.started.v1"   // Capture
    SchemaExecutionCompleted = "shepherd2.execution.completed.v1" // Capture
    SchemaExecutionFailed    = "shepherd2.execution.failed.v1"    // Capture
)

type ExecutionStatus string // pending | running | succeeded | failed

type Execution struct {
    ExecutionID, TaskRef, ParentExecutionID string
    Status     ExecutionStatus
    Inputs, Outputs map[string]any
    Error      string
    StartedFactID, TerminalFactID string
    Cutoff     *Frontier // nil until terminal frontier published
}

func ExecutionIDFor(appendIntentID, localRef string) (string, error) // "exec:<32 hex>"
func ExecutionCreatedDraft(...) RecordDraft  // declaration; created IS the ID anchor
func ExecutionStartedDraft(...) RecordDraft
func ExecutionCompletedDraft(...) RecordDraft
func ExecutionFailedDraft(...) RecordDraft
func CreateExecutionBatch(execID, taskRef, parentID string, inputs map[string]any) AppendBatch
func CompleteExecutionBatch(execID string, outputs map[string]any) AppendBatch
func FailExecutionBatch(execID string, errMsg string) AppendBatch
func PublishExecutionFrontier(ctx AppendContext, store *SQLiteTraceStore, spec ... ) (Frontier, error)
    // terminal-frontier law: through-fact must be completed/failed schema — validate before publish
func ProjectExecution(slice Slice, ownerID string, cutoff *Frontier) (*Execution, error)
    // owner-prefix fold over the owner path: created → started → terminal
func ProjectExecutionFromStore(ctx ReadContext, store *SQLiteTraceStore, execID string) (*Execution, error)
```

**Parity detail to verify against Python at implementation time** (do not
guess): exact `execution_id_for` input shape (canonical JSON of
`{append_intent_id, local_ref}`? concat?), payload key order/names, which
fields the fold tolerates as missing, and the frontier spec's
`caused_by` wiring. Pin each with cross-language vectors (§6).

## 2. Relations (relations.go)

```go
const SchemaExecutionRelation = "shepherd2.execution_relation.created.v1"
type RelationKind string // spawned | adopted | abandoned

type ExecutionRelation struct { RelationID, ParentExecutionID, ChildExecutionID string; Kind RelationKind }

func RelationIDFor(...) (string, error)
func CreateExecutionRelationBatch(parentID, childID string, kind RelationKind) AppendBatch
func ProjectExecutionRelations(slice Slice, ...) ([]ExecutionRelation, error)
func ProjectExecutionRelationsFromStore(ctx ReadContext, store *SQLiteTraceStore, ...) ([]ExecutionRelation, error)
```

Parent-owned facts (relations live on the parent's owner path) — assert this
in tests; it is what makes `project_effective_history` work.

## 3. Effective history (history.go)

```go
type PublishedFact struct { /* mirror schemas/history.py PublishedFact */ }
type EffectiveChild struct { /* ExecutionRelation + child Execution projection */ }
type EffectiveHistory struct {
    RootExecutions []Execution
    ActiveChildren []EffectiveChild
    PublishedFacts []PublishedFact
}
func ProjectEffectiveHistory(slice Slice) (*EffectiveHistory, error)
func ProjectEffectiveHistoryFromStore(ctx ReadContext, store *SQLiteTraceStore, frontierID string) (*EffectiveHistory, error)
```

This is the "what did the run tree actually do" view — root executions +
active children + parent-published facts resolved from a frontier.

## 4. Schema library / projection purity (schema_library.go)

```go
type ProjectionModeRequirement string // declarations_only | captures_only | both | any
type ProjectionSpec struct { Name string; ModeRequirement ProjectionModeRequirement }

type SchemaLibrary interface {
    Name() string
    SchemaRefs() []string
    ProjectionSpec(name string) (ProjectionSpec, bool)
}
type StaticSchemaLibrary struct{ ... }
func NewStaticSchemaLibrary(name string, schemaRefs []string, specs map[string]ProjectionSpec) *StaticSchemaLibrary

var ErrProjectionMode = errors.New("shepherd: projection mode incompatible")
func EnsureProjectionCompatible(slice Slice, spec ProjectionSpec) error
```

Used by `ProjectExecution` (and plan 04's settlement projections) to fail
loudly when a slice's mode filter contradicts the projection's requirements —
this also closes the plan-01 law "projections fail on incompatible mode
filter".

Default library: `ShepherdSchemas()` static library registering execution +
relations + history refs and their specs.

## 5. Runtime handles (handles.go)

Sync facade over the store, mirroring `runtime/handles.py` (no scheduler, no
goroutine management inside the package — callers may wrap in goroutines):

```go
type TaskFunc func(execCtx context.Context, control *TaskControl) (map[string]any, error)

type Run struct { ... } // started execution; from StartTask
func (r *Run) ExecutionID() string
func (r *Run) Wait(ctx context.Context) (*Execution, error)  // blocks until terminal (poll or channel fed by bus — decide at impl time; prefer a done channel closed by the runner goroutine)
func (r *Run) Snapshot(ctx context.Context) (*Execution, error) // projected current state
func (r *Run) Cutoff() *Frontier

type ChildHandle struct { ... } // spawned execution observed from parent
// same Wait/Snapshot/Cutoff surface

type TaskControl struct { ... } // handed to TaskFunc
func (c *TaskControl) CausalTail() []string
func (c *TaskControl) Publish(kind string, data map[string]any) error
    // "shepherd2.runtime.published_fact.v1" capture on the execution's owner path
func (c *TaskControl) Spawn(taskRef string, inputs map[string]any, fn TaskFunc) (*ChildHandle, error)
    // relation: spawned; child gets its own owner path + create/start batch
func (c *TaskControl) Adopt(childExecID string) error   // relation: adopted
func (c *TaskControl) Abandon(childExecID string) error // relation: abandoned
func (c *TaskControl) AwaitTerminal(ctx context.Context, h *ChildHandle) (*Execution, error)
func (c *TaskControl) ReadExecution(execID string) (*Execution, error)

type Registry struct{ ... } // taskRef → TaskFunc
func (reg *Registry) Register(taskRef string, fn TaskFunc)
func StartTask(ctx context.Context, store *SQLiteTraceStore, reg *Registry, taskRef string, inputs map[string]any) (*Run, error)
    // create+start batch → run fn in caller-managed goroutine or synchronously (mirror Python: @task start runs the body; keep a Sync option and an Async option; default Async with Wait)
    // body panic/error → FailExecutionBatch; success → CompleteExecutionBatch; then PublishExecutionFrontier
```

Design notes:
- Python is strictly synchronous; the Go facade offers both (`StartTaskSync`
  runs the body inline before returning — closest to Python; `StartTask`
  returns a live `Run` whose body executes in a goroutine). Document the
  divergence as a Go-idiom addition, not an ABI change (record shapes are
  identical).
- Owner-path convention per execution: `exec-owner:<execution_id>` — verify
  Python's owner naming in `handles.py`/tests and mirror it; owner paths are
  visible in traces and slices must resolve cross-language.

## 6. Golden vectors & tests

- **Vectors** (extend `testdata/`, Python-generated per plan 01 §3 process):
  `execution_id_for` derivations (several intent/local-ref pairs), relation IDs,
  published-fact schema payload shape, a full create→start→complete batch
  sequence with expected record IDs and frontier payload.
- **Unit tests**: draft builders produce expected schema refs/modes/kind
  labels; fold from hand-built slices; terminal-frontier law rejects
  non-terminal through-facts; projection purity errors; relation batching
  lands on parent owner path.
- **Integration test** (`handles_integration_test.go`): register a task that
  spawns two children (one adopted mid-flight, one abandoned), publishes a
  fact, completes; assert `ProjectEffectiveHistory` from the run frontier shows
  the expected tree; assert `Wait`/`Snapshot` transitions
  pending→running→succeeded; assert fail path records `failed` + publishable
  frontier.
- **Restart law**: reopen the store file, re-project everything — identical.

## 7. Acceptance criteria

- [ ] Execution/relation/history projections pass Python-generated vectors.
- [ ] `StartTaskSync` record trace is ID-identical to Python `@task` run for
      the same fixture task (vector-pinned).
- [ ] Terminal-frontier law + projection purity enforced with negative tests.
- [ ] `go test ./... -count=1` green; no new dependencies in go.mod.
- [ ] README gains a "Executions & handles" section (short, links to godoc).
