# shepherd-kernel-go

Go port of the [Shepherd](https://github.com/shepherd-agents/shepherd) trace kernel — a content-addressed, append-only, causally-linked execution trace store over SQLite.

## What It Is

The kernel records agent actions as durable, inspectable, reversible traces. Every tool call, every file mutation, every agent decision becomes a record in a causal DAG with content-addressed identity.

This is a standalone Go module implementing `shepherd.kernel.abi.v0` — the same frozen ABI as the Python reference (`shepherd2`) and produces byte-identical digests from the same golden test vectors.

## Install

```bash
go get github.com/buchenberg/shepherd-kernel-go
```

## Quick Start

```go
package main

import (
    "fmt"
    shepherd "github.com/buchenberg/shepherd-kernel-go"
)

func main() {
    store, _ := shepherd.NewSQLiteTraceStore("trace.sqlite")
    defer store.Close()

    // Record a tool call as a declaration (intent)
    receipt, _ := store.Append(shepherd.TrustedAppendContext, shepherd.AppendBatch{
        AppendIntentID: "session:tool:1",
        Groups: []shepherd.AppendGroup{{
            TraceOwnerID: "session:abc",
            FactDrafts: []shepherd.RecordDraft{{
                Mode:      shepherd.Declaration,
                SchemaRef: "myapp.tool.edit.v1",
                Payload:   map[string]any{"file": "main.go", "old": "...", "new": "..."},
            }},
        }},
    })

    // Record the result as a capture (observation)
    store.Append(shepherd.TrustedAppendContext, shepherd.AppendBatch{
        AppendIntentID: "session:result:1",
        Groups: []shepherd.AppendGroup{{
            TraceOwnerID:  "session:abc",
            CausalParents: receipt.FactIDs,
            FactDrafts: []shepherd.RecordDraft{{
                Mode:      shepherd.Capture,
                SchemaRef: "myapp.tool.edit.v1.applied",
                Payload:   map[string]any{"success": true},
            }},
        }},
    })

    // Read back the trace
    slice, _ := store.ReadOwnerPrefix(
        shepherd.ReadContext{ActorRef: "reader"},
        "session:abc", 99, shepherd.ModeBoth,
    )
    fmt.Println(slice.FactIDs())

    // Publish a checkpoint
    frontier, _ := store.PublishFrontier(shepherd.TrustedAppendContext, shepherd.FrontierSpec{
        FrontierID:         "frontier:checkpoint",
        TargetTraceOwnerID: "session:abc",
        ThroughFactId:      receipt.FactIDs[len(receipt.FactIDs)-1],
    })

    // Later: resolve the checkpoint
    checkpoint, _ := store.ResolveFrontier(
        shepherd.ReadContext{ActorRef: "reader"},
        frontier.FrontierId, shepherd.ModeBoth,
    )
    fmt.Println(checkpoint.FactIDs())
}
```

## Core Concepts

### Records

Every trace entry is a **record** with content-addressed identity — `record_id == SHA-256(canonical_input)`. Two records with identical content produce the same ID, regardless of when or where they were created.

Records have a **mode**:
- `declaration` — an intention (what the agent proposed)
- `capture` — an observation (what actually happened)

### Witnesses

Records cite a **witness** that describes the authority and environment under which they were accepted. Witnesses chain to a deterministic root witness (genesis record).

### Causal Edges

Records declare their causal parents via `caused_by`. Parent order is part of record identity — changing the order changes the ID. This creates a DAG of provenance.

### Frontiers

A **frontier** is an immutable bookmark pointing to a specific record on a specific owner path. Once published, later records don't mutate it. This enables point-in-time reads and undo.

### Slices

A **slice** is a graph-shaped read result — the selected records, their causal edges, witness support, and anchors for out-of-slice evidence.

## API

### Store

```go
store, err := shepherd.NewSQLiteTraceStore(path)  // ":memory:" for in-memory
defer store.Close()

receipt, err := store.Append(ctx, batch)            // Append records
slice, err := store.ReadOwnerPrefix(ctx, owner, through, modeFilter)
slice, err := store.ReadCausalClosure(ctx, roots, modeFilter, closurePolicy)
frontier, err := store.PublishFrontier(ctx, spec)   // Immutable checkpoint
slice, err := store.ResolveFrontier(ctx, frontierId, modeFilter)
cutoff, err := store.ReadOwnerCutoff(frontierId)
ids, err := store.PreviewRecordIDs(ctx, batch)      // Dry-run append
```

### Constants

```go
shepherd.Capture          // "capture"
shepherd.Declaration      // "declaration"
shepherd.ModeBoth         // "both"
shepherd.ModeCapturesOnly // "captures_only"
shepherd.ModeDeclarationsOnly // "declarations_only"
shepherd.TrustedAppendContext  // Default trusted append context
shepherd.TrustedReadContext    // Default trusted read context
```

### Golden Vectors

The `testdata/kernel_abi_v0.json` file contains deterministic test vectors shared across all three implementations. If your digest of the root witness body matches `sha256:28aa527e...`, your implementation is compatible.

## Tests

```bash
go test -v ./...
```

38 tests covering:
- Golden vector digest compatibility (21 tests)
- Append idempotency and conflict detection
- Content-addressed identity across owners
- Cut/frontier publish and resolve roundtrip
- Causal closure with parent traversal
- Mode filtering (capture/declaration/both)
- Witness chain validation to root
- Authorization enforcement
- Persistence across store restarts

## Dependencies

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- Go 1.21+

## License

MIT
