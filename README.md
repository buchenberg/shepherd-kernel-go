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

### Workspace State

A **`WorkspaceState`** is a backend-neutral snapshot of a workspace — the replacement for the git-specific stash/HEAD pair:

```go
type WorkspaceState struct {
    Backend  string         // "git"