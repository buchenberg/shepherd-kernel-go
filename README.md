# shepherd-kernel-go

Go implementation of the [Shepherd](https://arxiv.org/abs/2605.10913) trace kernel with real-time effect bus, execution scopes, and meta-agent supervision.

A content-addressed, append-only, causally-linked execution trace store over SQLite — plus the primitives needed to supervise agent execution in real time.

## What It Is

The kernel records agent actions as durable, inspectable, reversible traces. Every tool call, every file mutation, every agent decision becomes a record in a causal DAG with content-addressed identity.

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
mgr := shepherd.NewScopeManager(store, bus)

parent, _ := mgr.Create("agent:main")
child, _ := mgr.Fork(parent.ID(), "agent:experimental")

// Child runs in isolation...
// If it works:
parent.Merge(child)
// If it fails:
parent.Discard(child)  // child's records remain, but semantically abandoned
```

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

## Architecture

```
┌─────────────────────────────────────────────┐
│  Orchestrator                               │
│  ┌───────────────────────────────────────┐  │
│  │ Supervisor                            │  │
│  │  Rules → Interventions channel        │  │
│  └──────────┬────────────────────────────┘  │
│             │                               │
│  ┌──────────▼────────────────────────────┐  │
│  │ ScopeManager                          │  │
│  │  Fork / Merge / Discard               │  │
│  └──────────┬────────────────────────────┘  │
│             │                               │
│  ┌──────────▼────────────────────────────┐  │
│  │ EffectBus                             │  │
│  │  Non-blocking pub/sub                 │  │
│  └──────────┬────────────────────────────┘  │
│             │                               │
│  ┌──────────▼────────────────────────────┐  │
│  │ SQLiteTraceStore                      │  │
│  │  Content-addressed, append-only       │  │
│  └───────────────────────────────────────┘  │
└─────────────────────────────────────────────┘
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
mgr := shepherd.NewScopeManager(store, bus)

scope, err := mgr.Create("owner-id")
scope, err := mgr.Fork("parent-scope-id", "child-owner-id")
err := mgr.Merge("child-scope-id")
err := mgr.Discard("child-scope-id")
scope, ok := mgr.Get("scope-id")
active := mgr.ActiveScopes()
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
```

## Golden Vectors

The `testdata/kernel_abi_v0.json` file contains deterministic test vectors shared across implementations. If your digest of the root witness body matches `sha256:28aa527e...`, your implementation is compatible.

## Tests

```bash
go test -v ./...
```

89 tests covering:
- Golden vector digest compatibility (21 tests)
- Append idempotency and conflict detection
- Content-addressed identity across owners
- Cut/frontier publish and resolve roundtrip
- Causal closure with parent traversal
- Mode filtering (capture/declaration/both)
- Witness chain validation to root
- Authorization enforcement
- Persistence across store restarts
- Effect bus pub/sub, non-blocking, concurrency (15 tests)
- Scope fork/merge/discard, nesting, lifecycle (20 tests)
- Supervisor rules, interventions, built-in rules (16 tests)

## Dependencies

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- Go 1.21+

## License

MIT
