# shepherd-kernel-go

Go implementation of the [Shepherd](https://arxiv.org/abs/2605.10913) trace kernel with real-time effect bus, execution scopes, and meta-agent supervision.

A content-addressed, append-only, causally-linked execution trace store over SQLite — plus the primitives needed to supervise agent execution in real time.

## What It Is

The kernel records agent actions as durable, inspectable, reversible traces. Every tool call, every file mutation, every agent decision becomes a record in a causal DAG with content-addressed identity.

Reversibility is real, not notional: scopes carry a pluggable **Sandbox** that captures, restores, and diffs the workspace they operate on, so a speculative branch can be rolled back or discarded outright. Two backends ship today — an in-place git materializer that works on any platform, and a git-worktree backend that isolates each branch in its own checkout.

This is a standalone Go module implementing `shepherd.kernel.abi.v0` — the same frozen ABI as the Python reference (`shepherd2`) and produces byte-identical digests from the same golden test vectors.

## Install

```bash
go get github.com/buchenberg/shepherd-kernel-go
```

## What You Can Do With It

### Record agent execution traces

Every tool call, every model response, every file mutation becomes a durable, content-addressed record in a causal DAG:

```go
store, _ := shepherd.NewSQLiteTraceStore("trace.sqlite")
defer store.Close()

// Record a tool call intent
receipt, _ := store.Append(shepherd.TrustedAppendContext, shepherd.AppendBatch{
    AppendIntentID: "session:tool:1",
    Groups: []shepherd.AppendGroup{{
        TraceOwnerID: "agent:main",
        FactDrafts: []shepherd.RecordDraft{{
            Mode:      shepherd.Declaration,
            SchemaRef: "app.tool.bash.v1",
            Payload:   map[string]any{"cmd": "go test ./..."},
        }},
    }},
})

// Record the outcome
store.Append(shepherd.TrustedAppendContext, shepherd.AppendBatch{
    AppendIntentID: "session:result:1",
    Groups: []shepherd.AppendGroup{{
        TraceOwnerID:  "agent:main",
        CausalParents: receipt.FactIDs,
        FactDrafts: []shepherd.RecordDraft{{
            Mode:      shepherd.Capture,
            SchemaRef: "app.tool.bash.v1.applied",
            Payload:   map[string]any{"success": true, "duration": "2.3s"},
        }},
    }},
})
```

### Observe agent actions in real time

Subscribe to the effect bus to receive events as they happen — no polling, no SQLite reads:

```go
bus := shepherd.NewEffectBus(64)
store.WithBus(bus)

ch := bus.Subscribe("my-watcher")

// Events arrive as records are appended
event := <-ch
fmt.Println(event.KindLabel)  // "bash"
fmt.Println(event.Payload)    // map[cmd:go test ./...]
```

### Fork and branch execution

Create isolated execution branches for speculative work. Fork records the branch point in the trace; merge or discard the branch later:

```go
mgr := shepherd.NewScopeManager(store)

// A root scope carries the workspace it operates on.
parent, _ := mgr.Create("agent:main", shepherd.NewLocalGitSandbox(repoPath))
child, _ := mgr.Fork(parent.ID(), "agent:experimental", nil)

// Child runs in isolation...
// If it works:
parent.Merge(child)
// If it fails:
parent.Discard(child)  // child's records remain, but semantically abandoned
```

### Roll the workspace back

Checkpoints capture workspace state plus an opaque caller snapshot (typically
conversation history). A restore rewinds both:

```go
cp, _ := mgr.CreateCheckpoint(ctx, scope.ID(), conversationSnapshot)

// ...the sub-agent makes destructive changes...

snapshot, _ := mgr.RestoreCheckpoint(ctx, cp.ID)  // workspace reverted, snapshot returned
// Checkpoints are single-use: restore marks them used and releases the snapshot.
```

Reusable (non-consuming) workspace states are available when you need to apply
the same state more than once — that is what fork-and-choose requires:

```go
forkState, _ := scope.CaptureWorkspace(ctx)   // no lifecycle, stays valid
// ...variant A...
scope.ApplyWorkspace(ctx, forkState)          // rewind to the fork point
// ...variant B...
```

### Isolate a branch in its own worktree

The git backend has two modes. In-place operates on your repository directly.
Worktree allocates a detached `git worktree` per scope, so a speculative branch
cannot touch the parent tree and `Discard` becomes a physical rollback:

```go
sb := shepherd.NewWorktreeSandbox(repoPath, worktreePath)
sb.Create(ctx, shepherd.SandboxSpec{})
defer sb.Destroy(ctx)

// Scope-scoped ownership: the sandbox is destroyed by Discard/Merge/Halt.
child, _ := mgr.ForkIsolated(parent.ID(), "agent:experimental", nil, sb)
```

Forking is `Create` then `Apply(forkState)`: the worktree lands on the fork HEAD,
then the fork-point state is replayed so the variant sees the parent's exact
(uncommitted) tree. Captured state survives teardown because worktrees share the
repository's object store.

### Supervise sub-agents

Define rules that watch for dangerous patterns and intervene automatically:

```go
sup := shepherd.NewSupervisor(mgr, bus)
sup.AddRule(shepherd.DestructiveToolRule())            // block rm/mv/chmod
sup.AddRule(shepherd.HighErrorRateRule(0.5, 10))       // alert at 50% errors
sup.AddRule(shepherd.StuckDetectionRule(3))            // detect loops
sup.Start("orchestrator")

for iv := range sup.Interventions() {
    switch iv.Type {
    case shepherd.InterventionInject:
        sup.Inject(iv.ScopeID, "try a different approach")
    case shepherd.InterventionHalt:
        sup.Halt(iv.ScopeID)
    }
}
```

### Inspect execution history

Read back traces by owner, causal closure, or frontier checkpoint:

```go
// Everything an agent did
slice, _ := store.ReadOwnerPrefix(ctx, "agent:main", 999, shepherd.ModeBoth)

// Causal closure from a specific record
slice, _ = store.ReadCausalClosure(ctx, []string{"sha256:abc"}, shepherd.ModeBoth, "include_external_anchors")

// Point-in-time checkpoint
frontier, _ := store.PublishFrontier(ctx, shepherd.FrontierSpec{...})
checkpoint, _ := store.ResolveFrontier(ctx, frontier.FrontierID, shepherd.ModeBoth)
```

### Materialize recorded intents

Declarations are commitments: records that say what an agent *intended* to do.
Materialization is the other half of that rhythm — applying recorded
declarations through a typed substrate and appending the receipts as
captures, so the trace describes both sides of the transition:

```go
registry := shepherd.NewSubstrateRegistry()
registry.Register(shepherd.NewWorkspaceSubstrate(sandbox))  // or a KV, echo, custom substrate

receipt, err := shepherd.Materialize(ctx, store,
    appendCtx.ToOperationContext(shepherd.OpMaterialize),
    shepherd.MaterializationRequest{
        AppendIntentID:            "run:42:materialize",
        TargetTraceOwnerID:        "agent:main",
        TargetRecordIDs:           declarationIDs, // workspace.file.write.v1 records
        TargetThroughOwnerOrdinal: shepherd.MaxOwnerOrdinal,
    },
    registry,
)
// receipt.Outcome, receipt.ProducedRecordIDs — applied captures, caused by
// their declarations, witness-stamped with the substrate that applied them.
```

The transition is idempotent once an intent has *completed*: a same-intent
retry replays the stored receipt from the completed-intent ledger without
redispatching the substrate, and a same-intent request with different content
is a conflict, not a silent second write. Dispatch itself is at-least-once:
in the window between the substrate call and the ledger write — including a
crash there — the substrate runs again. Deterministic substrates (the
reference Echo and KV substrates) converge to the identical receipt in that
window; a non-deterministic one fails loudly as an intent conflict rather
than double-writing. The substrate is selected by the declarations' own
witnesses — the `substrate_ref` stamped when they were appended — never
guessed from schemas.

`WorkspaceSubstrate` bridges the kernel's `Sandbox` interface, so recorded
file-write and exec intents reach a real workspace (git, worktree, or
containerd) through the same declare→capture rhythm, gated on the backend's
capabilities: the git backend honestly reports `clean_failure` instead of
half-applying. Outcome honesty is the contract: `clean_failure` is claimed
only when nothing was attempted — a failure *after* a sandbox call was made
is reported as `split_state` even when no capture landed, because backends
write files in chunks and the world may have changed without a trace. The
reference KV substrate (`shepherd.NewKVSubstrate`) keeps a world-side SQLite
store separate from the trace, and the echo substrate is the test fixture.

## Architecture

```
┌──────────────────────────────────────────────────┐
│  Orchestrator                                    │
│  ┌────────────────────────────────────────────┐  │
│  │ Supervisor                                 │  │
│  │  Rules → Interventions channel             │  │
│  └──────────┬─────────────────────────────────┘  │
│             │                                    │
│  ┌──────────▼─────────────────────────────────┐  │
│  │ ScopeManager                               │  │
│  │  Fork / ForkIsolated / Merge / Discard     │  │
│  └────┬──────────────────────────────┬────────┘  │
│       │                              │           │
│  ┌────▼──────────────────┐  ┌────────▼────────┐  │
│  │ Sandbox               │  │ EffectBus       │  │
│  │  Capture / Apply /    │  │  Non-blocking   │  │
│  │  Diff / Exec          │  │  pub/sub        │  │
│  │  git · containerd     │  └────────┬────────┘  │
│  └───────────────────────┘           │           │
│                                      │           │
│  ┌───────────────────────────────────▼────────┐  │
│  │ SQLiteTraceStore                           │  │
│  │  Content-addressed, append-only            │  │
│  └────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────┘
```

## Core Concepts

### Records

Every trace entry is a **record** with content-addressed identity — `record_id == SHA-256(canonical_input)`. Two records with identical content produce the same ID.

Records have a **mode**:
- `declaration` — an intention (what the agent proposed)
- `capture` — an observation (what actually happened)

### Witnesses

Records cite a **witness** that describes the authority and environment under which they were accepted. Witnesses chain to a deterministic root witness (genesis record).

### Causal Edges

Records declare their causal parents via `caused_by`. Parent order is part of record identity. This creates a DAG of provenance.

### Frontiers

A **frontier** is an immutable bookmark pointing to a specific record on a specific owner path. Enables point-in-time reads and undo.

### Slices

A **slice** is a graph-shaped read result — the selected records, their causal edges, witness support, and anchors for out-of-scope evidence.

### Effect Bus

A transient pub/sub layer that fans out `EffectEvent` messages to subscribers. Non-blocking — slow subscribers get their oldest event dropped, never block the writer.

### Scopes

A **scope** represents a branch of execution. Fork creates a child scope with its own trace owner; merge propagates causal links; discard abandons the branch. Lifecycle events are recorded in the trace.

A scope also carries an optional **sandbox** — its execution substrate. A scope created without one is *pure-causal*: it participates in the trace DAG but workspace operations return `ErrNoSandbox`. `Fork` inherits the parent's sandbox without owning it; `ForkIsolated` attaches a new sandbox that the child owns, so `Discard`/`Merge`/`Halt` destroy it.

### Sandboxes

A **Sandbox** is the substrate a scope forks, snapshots, restores, diffs, and executes in. It is the seam that keeps the kernel independent of any particular isolation technology:

```go
type Sandbox interface {
    Backend() string
    Capabilities() SandboxCapabilities
    Create(ctx context.Context, spec SandboxSpec) error
    Destroy(ctx context.Context) error
    Capture(ctx context.Context) (WorkspaceState, error)
    Apply(ctx context.Context, ws WorkspaceState) error
    Diff(ctx context.Context, ws WorkspaceState, maxLines int) (string, []string, error)
    Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}
```

Backends need not implement everything. `Capabilities()` reports what is available, and unimplemented operations return `ErrUnsupported` rather than pretending to succeed. That is how the git backend — a materializer, not an isolation boundary — satisfies the interface without claiming `Exec` or `Isolated`.

`Capture` and `Diff` are **non-mutating**: the caller's git index is left as it was found.

Two backends ship in the core module:

| Backend | Mode | Lifecycle | Isolated | Containment |
|---|---|---|---|---|
| `git` | in-place (`NewLocalGitSandbox`) | No — validates only | No | `uncontained` |
| `git` | worktree (`NewWorktreeSandbox`) | Yes — real worktrees | Yes | `buffered` |

A third backend lives in the nested module `sandbox/containerd/`: an
overlay-snapshotter backend with namespace isolation. Its snapshot lifecycle,
teardown ordering, layer pruning, file I/O, and diff are implemented and
unit-tested against containerd's real snapshotter interface.

**Daemon status: verified.** The live harness
(`sandbox/containerd/live_test.go`, env-gated on `SHEPHERD_CONTAINERD_ADDR`) was
first run against a real containerd v2.3.5 in October 2026. It found and fixed
nine defects the fakes could not reach — exec never started its process, the
rootfs snapshot key was the manifest digest rather than the layer chainID,
teardown orphaned exec'd processes, and `Diff` mutated the caller's git index.
It then exposed a tenth: the adapter held **no containerd lease**, so once
`Capture` stopped the task its just-committed snapshot was unreferenced and the
daemon's garbage collector could reclaim it before the successor was prepared
(`mutation_threshold = 100`, `schedule_delay = 0s` on a default install). That
made the suite fail 3 runs in 5, a different test each time.

The sandbox now holds a labelled lease for its lifetime, and **12 consecutive
live runs are green**. The harness needs a Linux host with a running containerd
daemon; a stock image and the overlayfs snapshotter suffice, and no root-owned
FIFO directory is required. It is Linux-only and can never be this module's
default.

**State durability.** A first run against a *cold* daemon (fresh namespace, no
image) found an eleventh defect the warm-daemon runs had masked, and a twelfth
that no run could have passed: `Destroy` deleted the committed layers backing
captured states, so `Apply` after `Destroy` — plan 03's acceptance sequence —
failed every time. States are now durable by construction: every `Capture`
pins its committed layer under a namespace-scoped `shepherd-states` lease that
outlives any sandbox, `Destroy` removes only the sandbox's private active
layer, and an optional `StateReleaser` interface
(`ReleaseState`) is the deliberate reclaim path — disk comes back when the
caller says so, not when a sandbox happens to die. Promoting the release call
into core's `Sandbox` interface is a separate kernel-API decision.

### Workspace State

A **`WorkspaceState`** is a backend-neutral snapshot of a workspace — the replacement for the git-specific stash/HEAD pair:

```go
type WorkspaceState struct {
    Backend  string         // "git", "containerd"
    Revision string         // git HEAD SHA, or a snapshot key
    Data     map[string]any // backend-specific, opaque to the kernel
}
```

`Digest()` returns the kernel canonical digest of the state (`sha256:…`), used in trace records and for drift detection. Two states with identical content digest identically regardless of map iteration order, because canonicalization sorts keys at every level.

Unlike a `Checkpoint`, a `WorkspaceState` has no lifecycle: it is reusable and stays valid, so one fork point can seed many speculative branches.

### Supervisor

A rule-based engine that watches the effect bus and emits **interventions** when rules match. Built-in rules guard against destructive operations, high error rates, and stuck detection.

## API

### Store

```go
store, err := shepherd.NewSQLiteTraceStore(path)  // ":memory:" for in-memory
defer store.Close()
store.WithBus(bus)  // optional: attach effect bus

receipt, err := store.Append(ctx, batch)            // Append records
slice, err := store.ReadOwnerPrefix(ctx, owner, through, modeFilter)
slice, err := store.ReadCausalClosure(ctx, roots, modeFilter, closurePolicy)
frontier, err := store.PublishFrontier(ctx, spec)   // Immutable checkpoint
slice, err := store.ResolveFrontier(ctx, frontierId, modeFilter)
ids, err := store.PreviewRecordIDs(ctx, batch)      // Dry-run append
```

### Effect Bus

```go
bus := shepherd.NewEffectBus(bufferSize)  // 0 = default (64)
defer bus.Close()

ch := bus.Subscribe("watcher-id")     // <-chan EffectEvent
bus.Unsubscribe("watcher-id")
bus.Publish(event)                     // non-blocking
```

### Scope Manager

```go
mgr := shepherd.NewScopeManager(store)

scope, err := mgr.Create("owner-id", sb)                            // sb may be nil (pure-causal)
child, err := mgr.Fork("parent-scope-id", "child-owner-id", nil)    // inherits parent's sandbox
child, err := mgr.ForkIsolated("parent-scope-id", "child-owner-id", nil, sb)
err := parent.Merge(child)                                          // takes *Scope, not an ID
err := parent.Discard(child)
err := mgr.DestroyScopeSandbox(ctx, "scope-id")                     // release provisioned resources
scope, ok := mgr.Get("scope-id")
active := mgr.ActiveScopes()

cp, err := mgr.CreateCheckpoint(ctx, "scope-id", snapshot)          // single-use
snapshot, err := mgr.RestoreCheckpoint(ctx, cp.ID)
latest := mgr.LatestCheckpoint("scope-id")
mgr.PruneCheckpoints("scope-id")
```

### Scope workspace and checkpoint operations

```go
scope := shepherd.NewScope(store, "owner-id").WithSandbox(sb, false)  // owns=false: do not Destroy

ws, err := scope.CaptureWorkspace(ctx)          // reusable state, no lifecycle
err = scope.ApplyWorkspace(ctx, ws)
diff, files, err := scope.DiffWorkspace(ctx, ws, 2000)

cp, err := scope.CreateCheckpoint(ctx, snapshot)
snapshot, err := scope.RestoreCheckpoint(ctx, cp)
```

### Sandbox backends

```go
// In-place: materializes on your repository. Destroy is a no-op.
sb := shepherd.NewLocalGitSandbox(repoPath)

// Worktree: a detached git worktree this scope owns.
sb := shepherd.NewWorktreeSandbox(repoPath, worktreePath)

err := sb.Create(ctx, shepherd.SandboxSpec{})
defer sb.Destroy(ctx)

ws, err := sb.Capture(ctx)
err = sb.Apply(ctx, ws)
diff, files, err := sb.Diff(ctx, ws, maxLines)

caps := sb.Capabilities()   // capability negotiation: Lifecycle/Exec/FileIO/Diff/Isolated/Containment
```

### Supervisor

```go
sup := shepherd.NewSupervisor(mgr, bus)
sup.AddRule(shepherd.DestructiveToolRule())
sup.AddRule(shepherd.HighErrorRateRule(0.5, 10))
sup.AddRule(shepherd.StuckDetectionRule(3))
sup.Start("orchestrator")
defer sup.Close()

for iv := range sup.Interventions() { ... }

err := sup.Inject(scopeID, "guidance text")
err := sup.Halt(scopeID)
```

### Materialization

```go
registry := shepherd.NewSubstrateRegistry()
err := registry.Register(substrate)               // Substrate interface: SubstrateRef,
                                                  // DeclarationSchemas, CaptureSchemas,
                                                  // Containment, Materialize
substrate, err := registry.Get(substrateRef)      // ErrUnknownSubstrate when missing

receipt, err := shepherd.Materialize(ctx, store, opCtx, req, registry)
// opCtx.Operation must be shepherd.OpMaterialize
// req: AppendIntentID, TargetTraceOwnerID, TargetRecordIDs,
//      TargetThroughOwnerOrdinal (shepherd.MaxOwnerOrdinal), CaptureTraceOwnerID
// receipt: Outcome (success | clean_failure | split_state), SubstrateRef,
//          TargetRecordIDs, ProducedRecordIDs, WorldSideAnchors

echo := shepherd.NewEchoSubstrate("my.echo.v1", "example.write.v1")
kv, err := shepherd.NewKVSubstrate("world.sqlite")   // kv.sqlite.local.v1
ws := shepherd.NewWorkspaceSubstrate(sandbox)       // workspace.sandbox.v1

result, err := substrate.Materialize(ctx, records)   // substrates never append;
                                                     // dispatch owns the append
```

### Constants

```go
shepherd.Capture                    // "capture"
shepherd.Declaration                // "declaration"
shepherd.ModeBoth                   // "both"
shepherd.ModeCapturesOnly           // "captures_only"
shepherd.ModeDeclarationsOnly       // "declarations_only"
shepherd.TrustedAppendContext       // Default trusted append context
shepherd.TrustedReadContext         // Default trusted read context
shepherd.SchemaScopeForked          // "shepherd.scope.forked.v1"
shepherd.SchemaScopeMerged          // "shepherd.scope.merged.v1"
shepherd.SchemaScopeDiscarded       // "shepherd.scope.discarded.v1"
shepherd.SchemaSupervisorInject     // "shepherd.supervisor.inject.v1"
shepherd.SchemaSupervisorHalt       // "shepherd.supervisor.halt.v1"
shepherd.SchemaCheckpointCreated    // "shepherd.checkpoint.created.v2"
shepherd.SchemaCheckpointRestored   // "shepherd.checkpoint.restored.v2"
shepherd.SchemaWorkspaceCaptured    // "shepherd.workspace.captured.v1"
shepherd.SchemaWorkspaceApplied     // "shepherd.workspace.applied.v1"

shepherd.CheckpointValid            // "valid"
shepherd.CheckpointUsed             // "used"
shepherd.CheckpointInvalid          // "invalid"

shepherd.ErrNoSandbox               // scope has no sandbox
shepherd.ErrUnsupported             // backend does not implement the capability
```

### Trace schemas

The checkpoint and workspace schemas embed a backend-neutral `revision` and
`state_digest` rather than git-specific SHAs, so a trace reads the same whether
the workspace was a git tree, a worktree, or a container snapshot:

| Schema | Mode | Payload |
|---|---|---|
| `shepherd.checkpoint.created.v2` | declaration | `checkpoint_id, backend, revision, state_digest, has_snapshot` |
| `shepherd.checkpoint.restored.v2` | capture | `checkpoint_id, backend, revision, state_digest` |
| `shepherd.workspace.captured.v1` | declaration | `backend, revision, state_digest` |
| `shepherd.workspace.applied.v1` | capture | `backend, revision, state_digest` |

These are kernel extensions, not part of `shepherd.kernel.abi.v0`, so they do not
affect the golden vectors. Trace writes are advisory: a failed append never fails
the workspace operation, because the rollback guarantee does not depend on the
audit record.

## Executions & Handles

Executions give runs identities: a create/complete/fail lifecycle folded from
retained facts, parent/child relations, and a synchronous task facade over the
store. Every id is derived exactly as the Python reference derives it —
`ExecutionIDFor` and the full run sequence are pinned by Python-generated
vectors — so a Go-written trace is readable by `shepherd2` and vice versa.

```go
reg := shepherd.NewRegistry()
reg.Register("MyTask", func(c *shepherd.TaskControl) (map[string]any, error) {
    c.Publish("note", map[string]any{"msg": "working"})
    child, err := c.Spawn("LeafTask", inputs, reg)
    if err != nil { return nil, err }
    exec, err := c.AwaitTerminal(ctx, child)
    return map[string]any{"child": exec.ExecutionID}, nil
})

run, err := shepherd.StartTaskSync(store, reg, "MyTask", "run:fixed-id", inputs)
exec, err := run.Wait(ctx)      // terminal projection: succeeded / failed

// or async: StartTask returns a live handle; Wait blocks until terminal
run, err = shepherd.StartTask(ctx, store, reg, "MyTask", "", inputs)

history, err := shepherd.ProjectEffectiveHistoryFromStore(store, reader, cutoff)
// root execution + active children (spawned/adopted; abandoned drops) + published facts
```

Spawns and adopts are recorded as parent-owned relation facts; abandoning a
child writes an abandoned relation under the same relation id, so the
effective-history fold drops it. The whole tree projects from one terminal
frontier, and re-opening the store file re-projects it identically.

Godoc: https://pkg.go.dev/github.com/buchenberg/shepherd-kernel-go

## Golden Vectors

The `testdata/kernel_abi_v0.json` file contains deterministic test vectors shared across implementations. If your digest of the root witness body matches `sha256:28aa527e...`, your implementation is compatible.

## Tests

```bash
go test -v ./...
```

161 tests, all passing:

| Area | Tests |
|---|---|
| Golden vector digest compatibility (`canonical_test.go`) | 21 |
| Store: append idempotency, conflict detection, content-addressed identity, frontier publish/resolve, causal closure, mode filtering, witness chains, authorization, persistence | 18 |
| Effect bus: pub/sub, non-blocking, concurrency | 14 |
| Scopes: fork/merge/discard, nesting, lifecycle, sandbox ownership | 34 |
| Supervisor: rules, interventions, built-in rules | 24 |
| Checkpoints: create/restore, single-use, index hygiene | 18 |
| Workspace states: capture/apply/diff, reusability, non-mutating contract | 12 |
| Git sandbox: in-place and worktree modes | 15 |
| Integration: end-to-end rollback and worktree fork/discard/choose | 5 |

## Dependencies

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo). The core module has exactly one direct dependency.
- Go 1.21+

The `sandbox/containerd/` adapter is a nested module with its own `go.mod`, so its (much larger) dependency graph never enters the core module. It is excluded from `go build ./...` and `go test ./...` here; build it from its own directory.

## License

No `LICENSE` file is currently present in this repository, so no rights are granted
for reuse or redistribution yet. A license should be added before this module is
consumed as a dependency or published. The sibling `yaah` project uses
`MIT OR Apache-2.0`.
