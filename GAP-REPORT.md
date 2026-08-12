# Shepherd → Go Port: Detailed Gap Report

## Executive Summary

The Go port (`shepherd-kernel-go`) has completed **Phase 1–3** of the
development plan: the trace kernel, effect bus, scope lifecycle, and
supervision rule engine are all implemented and tested (94 tests, all
passing). The yaah integration on `feat/shepherd-supervision` wires the
trace middleware, supervisor tool, and scope manager into the pipeline.

**However, the core capability the user wants — "a subagent makes a
change, the supervisor rolls it back and restarts from an earlier
point" — is not yet achievable.** The port has the *plumbing* (scopes,
bus, supervisor) but is missing the *mechanisms* that make rollback
actually work: checkpoint/restore, effect materialization, and
pre-commit interception. The `SupervisorTool` in yaah explicitly
comments: *"Fork/merge/discard are NOT exposed because yaah sub-agents
execute on the real filesystem without sandbox isolation."*

This report maps every Shepherd subsystem, identifies what's ported,
what's stubbed, and what's missing — grounded in line-by-line code
comparison.

---

## Subsystem Analysis

### 1. Trace Kernel — ✅ COMPLETE

| Aspect | Python | Go | Status |
|--------|--------|-----|--------|
| Content-addressed records | `shepherd_core/` | `store.go` (2158 lines) | ✅ Done |
| Canonical JSON + SHA-256 | `canonical.py` | `canonical.go` (355 lines) | ✅ Done |
| Declaration/Capture modes | `effects.py` | `types.go` RecordMode | ✅ Done |
| Causal DAG (caused_by) | `effects.py` | types.go CausedByIDs | ✅ Done |
| Frontiers (immutable bookmarks) | `scope/stream.py` | `store.go` PublishFrontier | ✅ Done |
| SQLite-backed, append-only | — | `modernc.org/sqlite` | ✅ Done |
| Idempotent appends | — | append_intent_id | ✅ Done |
| Witness chain to root | `effects.py` | WitnessBody | ✅ Done |
| Visibility profiles | — | shape_only/payload/full_internal | ✅ Done |

**Tests**: 38 kernel tests (store_test.go, canonical_test.go), golden
vector compatibility with Python/.NET implementations.

This is solid. The trace kernel is a faithful port.

---

### 2. Effects System — ⚠️ PARTIAL (bus done, typed effects missing)

#### What's ported

The Go `EffectEvent` (`effect.go`, 32 lines) is a **transient bus
message** — a lightweight notification carrying RecordID, TraceOwnerID,
Mode, SchemaRef, KindLabel, Payload. This is the right design for the
pub/sub layer.

The `EffectBus` (`bus.go`, 126 lines) is a clean non-blocking fan-out:
- Buffered channels per subscriber (default 64)
- Drop-oldest on overflow (prevents slow supervisor from blocking agent)
- Ordered per-publisher, interleaved across publishers
- Store integration via `WithBus()` — publishes after each committed append

This matches the Python "non-perturbing observation" property: the
effect stream is identical whether or not a meta-agent watches.

#### What's missing

The Python Shepherd has a **typed effect hierarchy** with
reversibility tiers. This is the foundation for rollback, and it's
entirely absent in Go.

**Python** (`shepherd_core/effects/effects.py`):
```python
# Concrete effect types with reversibility metadata
class FileWritten(Effect):      # reversibility = AUTO
class FileDeleted(Effect):      # reversibility = AUTO
class BashCommand(Effect):      # reversibility = NONE (audit-only)
class DatabaseWrite(Effect):    # reversibility = COMPENSABLE
class ContextMaterialized(Effect):  # tracks materialization success
```

**Python** reversibility tiers (`shepherd_core/types.py`):
```python
class ReversibilityLevel:
    AUTO = 0        # filesystem writes, sandbox state — roll back natively
    COMPENSABLE = 1  # database writes — use compensation handlers
    NONE = 2         # model calls, emails — irreversible (audit-only)
```

**Go**: No typed effects. No reversibility classification. The
`EffectEvent.Payload` is an untyped `map[string]any` — the supervisor
can pattern-match on SchemaRef strings ("yaah.tool.bash.v1"), but
there's no reversibility metadata to drive rollback decisions.

**Impact**: Without reversibility tiers, the system cannot determine
which effects are safe to undo vs. which are audit-only. This blocks
automatic rollback.

---

### 3. Scope System — ⚠️ PARTIAL (lifecycle done, state model missing)

#### What's ported

The Go `Scope` (`scope.go`, 349 lines) implements the lifecycle:
- `Fork(childOwnerID, snapshot)` — creates child scope, records
  "scope.forked" declaration, captures opaque snapshot
- `Merge(child)` — records "scope.merged" capture, marks child merged
- `Discard(child)` — records "scope.discarded" capture, marks child discarded
- `Inject(guidance)` — records supervisor inject declaration
- `Halt()` — marks scope as discarded, records halt declaration

`ScopeManager` (`scope_manager.go`, 142 lines) tracks all scopes in a
registry with concurrent-safe lookup.

The `snapshot any` field is a deliberate design choice: it's opaque so
the scope package doesn't import yaah types. Callers can store
`[]types.Message` or whatever they need.

**Tests**: Fork creates child with correct causal link, merge
propagates, discard marks child, concurrent safety — all verified.

#### What's missing

The Python Shepherd's `ImmutableScope` is a **functional core** that
implements the central invariant:

```
state(t) = fold(apply_effect, effects[0:t], initial_state)
```

This means scope state is **derivable from the effect stream** — you
can reconstruct any historical state by replaying effects. This is the
foundation for time-travel, speculative execution, and rollback.

**Python** (`shepherd_core/scope/model.py`, 215 lines):
```python
@dataclass(frozen=True)
class ImmutableScope:
    _id: str
    _bindings: tuple[ContextBinding, ...]  # name → ExecutionContext
    _stream: Stream                         # immutable effect stream
    _parent: ImmutableScope | None
    _origin_id: str                         # fork origin tracking

    def with_effect(self, effect) -> ImmutableScope:
        """Returns NEW scope with effect appended + applied to state."""
    def apply_effect(self, effect) -> ImmutableScope:
        """Routes effect to matching binding, calls context.apply_effect()."""
    def with_binding(self, name, context) -> ImmutableScope:
        """Returns NEW scope with added binding."""
```

**Python** `ContextBinding` and `ExecutionContext`: Each binding is a
named context (e.g., "workspace" → filesystem context, "database" → DB
context). Effects are routed to the matching context by binding_name.
Each context maintains its own state derived from applied effects.

**Go**: The Scope has **no bindings, no execution contexts, no
derivable state**. It's a lifecycle tracker — it knows parent/child
relationships and records events in the trace, but it doesn't carry
the actual execution state. The `snapshot any` field is a placeholder
for state, but nothing applies effects to it or derives state from it.

**Python** `Stream` (`shepherd_core/scope/stream.py`, 1172 lines):
```python
class Stream:
    """Immutable, append-only, with rich queries."""
    def append(self, effect) -> Stream
    def query(self, effect_type, task_name, ...) -> Iterator[EffectLayer]
    def direct(self) -> Iterator           # this scope only, not children
    def by_depth(self, depth) -> Iterator  # effects at specific depth
    def truncate_to(self, position) -> Stream  # for checkpoint restore
```

**Go**: No Stream abstraction. The trace store IS the stream, but it
lacks `truncate_to` (needed for rollback) and query-by-scope-depth.

**Python** containment model:
```
SANDBOX → SCOPE → MATERIALIZED → ESCAPED
```
Effects graduate through containment levels. Only contained effects
can be discarded freely. This is a safety invariant.

**Go**: No containment model. The scope comment says: *"this scope
does NOT carry filesystem state or context bindings — yaah sub-agents
are independent LLM loops, not shared-fs containers."* This was a
deliberate simplification, but it means discard() is purely
semantics — it marks the scope as discarded in the trace but doesn't
actually undo anything.

---

### 4. Checkpoint System — ❌ MISSING ENTIRELY

This is the **most critical gap** for the rollback feature.

**Python** (`_scope/_checkpoint.py`, CheckpointManager):
```python
class CheckpointManager:
    def create(self, name: str) -> Checkpoint:
        """Record stream position + binding count."""
        position = len(scope._stream)
        binding_count = len(scope._bindings)
        return Checkpoint(name, position, binding_count, fingerprint)

    def restore(self, checkpoint, *, keep_bindings=None,
                exclude_effect_types=None, strict=False):
        """THE ROLLBACK MECHANISM:
        1. Truncate stream to checkpoint position
        2. Remove bindings added after checkpoint
        3. Replay remaining effects to rebuild context state
        4. Validate fingerprint (detect drift)
        """
        truncated_stream = scope._stream.truncate_to(checkpoint._position)
        # ... rebuild bindings from scratch by replaying effects ...
```

Checkpoint restore works because **state is derivable from the effect
stream** (the fold invariant). You truncate the stream, then replay
effects from the beginning to rebuild state. The effects after the
checkpoint position are discarded — their changes vanish.

**Go**: No checkpoint system. No `truncate_to` on any stream/store.
No state replay capability. The `Scope.snapshot` field captures an
opaque value at fork time, but there's no mechanism to restore to it.

**What a Go port needs**:
1. A checkpoint creation method that records the current trace
   position (frontier) for a scope
2. A restore method that:
   - Creates a new scope from the checkpoint's frontier
   - In yaah's case: restores the conversation history from the snapshot
   - Does NOT need to replay filesystem effects if yaah uses
     workspace-level rollback instead (see §5)

---

### 5. Materialization System — ❌ MISSING ENTIRELY

This is the **second critical gap** — how filesystem changes get
applied and rolled back.

**Python** has a multi-layer materialization system:

#### Layer 1: File Deltas (`simple_workspace/delta.py`, 231 lines)
```python
class FileDelta:
    """Change to a single file — full info for apply + replay."""
    path: str
    operation: Literal["create", "modify", "delete"]
    encoding: Literal["full", "delta", "zlib_base64", "raw_base64"]
    content: bytes | None
    new_content_hash: str | None     # SHA-256 for verification
    old_content_hash: str | None     # drift detection
    # Diff-match-patch for text (90-99% size reduction)
    # Adaptive zlib+base64 for binary

class FileChangeset:
    """Collection of FileDeltas — like a git patch, applied atomically."""
    deltas: tuple[FileDelta, ...]
    sha256: str  # auto-computed
```

#### Layer 2: Materializer (`simple_workspace/materializer.py`, 177 lines)
```python
class SimpleWorkspaceMaterializer:
    def materialize(self, intent) -> MaterializationResult:
        """Apply changesets with backup/restore for atomic rollback."""
        # For each delta:
        #   1. Drift detection (SHA-256 comparison)
        #   2. Backup existing file to temp dir
        #   3. Apply delta (create/modify/delete)
        # On failure: _restore_from_backup()

    def _restore_from_backup(self, target_path, backup_dir):
        """Rollback: restore all backed-up files."""
```

#### Layer 3: Materialization Manager (`_scope/_materialization.py`, 434 lines)
```python
def order_bindings_by_reversibility(bindings):
    """Order: AUTO first, then COMPENSABLE, then NONE.
    Ensures if a non-reversible context fails,
    we can rollback the reversible contexts that succeeded."""

def rollback_completed_materializations(completed):
    """Rollback in reverse order. Called when a later step fails."""
    for binding, intent, result in reversed(completed):
        materializer.rollback(intent, result)
```

#### Layer 4: Container OverlayFS (`overlay_extractor.py`, 870 lines)
For containerized execution: OverlayFS upper layer → FileCreate /
FilePatch / FileDelete effects. Whiteout file detection for deletions.
This is the 134ms fork/revert mechanism from the paper.

**Go**: Zero materialization code. No file delta tracking. No
backup/restore. No OverlayFS integration. No drift detection.

**What yaah actually needs**: Since yaah sub-agents work on a real
filesystem (not containers), the realistic path is:

**Option A — Git-based rollback** (simplest, yaah already has git tools):
- Before subagent starts: `git stash create` or `git commit` to save state
- On rollback: `git reset --hard <checkpoint>` or `git stash pop`
- Pros: Uses existing tools, no new infrastructure
- Cons: Only works if the workspace is a git repo; doesn't handle
  untracked files well without extra config

**Option B — File delta tracking** (Shepherd-faithful):
- Wrap yaah's write/edit/delete tools to emit FileDelta effects
- Store deltas in the trace as capture records
- On rollback: apply reverse deltas (delete created files, restore
  old content for modified files, recreate deleted files)
- Pros: Works without git, precise, reversible
- Cons: More code, needs to handle binary files, doesn't handle
  side effects from bash commands

**Option C — OverlayFS / container isolation** (paper-faithful):
- Run each subagent in its own overlay mount
- Fork = new upper layer; revert = drop upper layer
- Pros: 134ms fork/revert, handles all filesystem changes including
  side effects from compiled binaries
- Cons: Requires Linux, root or user namespaces, significant
  infrastructure. Not viable on Windows (yaah's primary platform).

---

### 6. Supervision System — ⚠️ PARTIAL (rules done, interception missing)

#### What's ported

The Go `Supervisor` (`supervisor.go`, 370 lines) is a rules engine:
- Subscribes to the EffectBus
- Evaluates rules in order (first match wins)
- Emits Interventions to a channel

Three built-in rules:
- `DestructiveToolRule` — matches bash rm/mv/chmod, write, delete
- `HighErrorRateRule` — sliding window error rate with per-owner state
- `StuckDetectionRule` — repeated tool call detection with per-owner state

Intervention types: inject (push guidance), fork, discard, halt.

**Tests**: 14 supervisor tests covering rule matching, inject, halt,
scope lookup, all three built-in rules.

#### What's missing

The Python Shepherd has **two fundamentally different supervision
patterns**, and only one is partially ported:

**Pattern A — Runtime Supervisor (observe + intercept)** — This is
what the Go port approximates. But there's a critical timing issue:

In the Go port, the bus publishes events **after** the store append
succeeds (store.go line 134-171). The trace middleware records tool
calls in `PostTool` — **after** the tool has already executed. So the
supervisor sees the event **after the damage is done**. The
`DestructiveToolRule` can inject a warning, but it can't prevent the
destructive operation.

**Python Pattern B — Check-at-commit supervision**:
```python
# supervision.py — the supervisor DENIES by raising
def drafts_only_supervisor(proposed: SubstrateOperationProposed):
    effect = proposed.effect
    if isinstance(effect, (FileCreate, FilePatch)):
        if not effect.path.startswith("drafts/"):
            raise SupervisorDenied(effect=effect, reason="path outside ./drafts/")
    # If we return without raising, the operation is APPROVED
```

The key: in Python, `SubstrateOperationProposed` is emitted **before**
the effect materializes. The supervisor approves by returning, denies
by raising. The "reversible wrap discards the run scope, so the denied
work never reaches ground."

**Go**: No pre-commit gate. No `SubstrateOperationProposed`
equivalent. No way to deny an operation before it executes. The
middleware pipeline has `PrepareStep` (pre-model) and `PostTool`
(post-execution), but no pre-tool-execution hook where a supervisor
could intercept.

**What yaah needs for pre-commit interception**: A new middleware hook
like `PreTool(ctx, toolCall) (approved bool, reason string)` that fires
before tool dispatch. The supervisor's rules would need to run in this
hook, not on the bus. This is architecturally different from the
current bus-based approach.

---

### 7. yaah Integration — ⚠️ PARTIAL (wired but not functional for rollback)

#### What's done

- `pipeline/config.go`: `shepherd_trace` builder creates bus + scope
  manager, attaches bus to store, shares scope manager via
  `tools.SharedScopeManager` global ✅
- `pipeline/trace.go`: Records tool calls (declaration+capture), turn
  lifecycle, frontiers ✅
- `tools/supervisor.go`: list_scopes, inject, halt, status ✅
- `tools/subagent_trace.go`: list, profile ✅
- `tools/supervisor_shared.go`: SharedScopeManager global ✅

#### What's broken or missing

**Critical wiring bug** — Sub-agents don't use the shared bus/scope
manager. In `subagent_loop.go` (line 113-118):
```go
if cfg.TraceDir != "" {
    store, err := pipeline.NewShepherdTraceStore(filepath.Join(cfg.TraceDir, "trace.sqlite"))
    // ...
    traceMw := pipeline.NewShepherdTraceMiddleware(store, cfg.TraceSessionID)
    l.Middleware = append(l.Middleware, traceMw)
}
```

This opens a **separate SQLite connection** to the same trace.sqlite
file. While SQLite supports concurrent access (with busy_timeout), the
sub-agent's trace middleware has its own bus-less, scope-manager-less
store. Effects from sub-agents are **not published to the shared bus**
— the orchestrator's supervisor cannot observe them in real time.

The sub-agent should use the shared store (with bus attached) from the
pipeline config, not create its own.

**Missing Phase 4 deliverables** (from SHEPHERD-GO-PLAN.md):
- ❌ `live_effects.go` middleware (publish before execution)
- ❌ Pre-tool interception hook in the pipeline
- ❌ Snapshot capture/restore in the loop (for restart-from-checkpoint)
- ❌ Config fields for supervision rules
- ❌ Agent frame wiring for supervisor startup

**The SupervisorTool explicitly doesn't expose fork/merge/discard**:
```go
// Fork/merge/discard are NOT exposed because yaah sub-agents execute
// on the real filesystem without sandbox isolation.
```

This is the honest acknowledgment of the core gap: without
materialization (§5) or checkpoint restore (§4), fork/merge/discard
are empty operations — they record events in the trace but change
nothing in the real world.

---

## Gap Summary Matrix

| Capability | Python Shepherd | Go Port | yaah Integration | Blocks Rollback? |
|---|---|---|---|---|
| Trace kernel | ✅ | ✅ | ✅ | — |
| Effect bus (live events) | ✅ | ✅ | ⚠️ sub-agents not wired | No |
| Typed effects + reversibility | ✅ | ❌ | ❌ | **Yes** |
| Scope lifecycle (fork/merge/discard) | ✅ | ✅ | ⚠️ not exposed | No |
| Scope state model (fold invariant) | ✅ | ❌ | ❌ | **Yes** |
| Checkpoint create/restore | ✅ | ❌ | ❌ | **Yes (critical)** |
| File delta tracking | ✅ | ❌ | ❌ | **Yes (critical)** |
| Materialization (apply/rollback) | ✅ | ❌ | ❌ | **Yes (critical)** |
| Pre-commit interception | ✅ | ❌ | ❌ | **Yes** |
| Supervisor rules engine | ✅ | ✅ | ⚠️ post-facto only | No |
| Snapshot capture at fork | — | ✅ | ❌ not used | No |
| Sub-agent bus wiring | — | — | ❌ separate store | No |

---

## Recommended Path Forward

### Minimum Viable Rollback (3 components)

To achieve "subagent makes a change, supervisor rolls it back and
restarts from earlier point," you need exactly three things:

#### 1. Workspace Checkpoint via Git (~200 lines)

Since yaah already has git tools and sub-agents work on real
filesystems, use git as the materialization layer:

```go
// checkpoint.go
type WorkspaceCheckpoint struct {
    ScopeID    string
    CommitSHA  string  // git stash create or git commit
    Messages   []types.Message  // conversation snapshot
    CreatedAt  time.Time
}

func CreateCheckpoint(repoPath string, messages []types.Message) (*WorkspaceCheckpoint, error)
func RestoreCheckpoint(cp *WorkspaceCheckpoint) error  // git reset --hard + reload messages
```

- `CreateCheckpoint`: `git add -A && git stash create` (or commit on
  a shadow branch), capture conversation messages
- `RestoreCheckpoint`: `git reset --hard <sha>`, restore messages to
  the loop's context manager

This sidesteps the entire materialization system. Git IS the
materializer. It handles create/modify/delete/untracked files. The
trace kernel records what happened; git undoes it.

#### 2. Pre-Tool Interception Hook (~100 lines)

Add a `PreTool` phase to the middleware pipeline:

```go
// pipeline/middleware.go
type Middleware interface {
    // ... existing ...
    PreTool(ctx context.Context, call ToolCall) (*ToolCall, error)
    // Return error to BLOCK the tool call. Nil = approved.
}
```

The supervisor's rules run here, not on the bus. If a rule matches a
destructive intent, return an error — the tool never executes.

This requires moving rule evaluation from the async bus loop to the
synchronous tool dispatch path.

#### 3. Checkpoint/Restart Loop Integration (~150 lines)

Wire checkpoints into the sub-agent dispatch:

```go
// subagent_runner.go
func RunSubagentWithSupervision(...) {
    cp := CreateCheckpoint(repoPath, initialMessages)

    subagent := NewSubAgentLoop(...)
    result := subagent.Run(task)

    if supervisor.ShouldRollback(result) {
        RestoreCheckpoint(cp)
        // Re-run with injected guidance from supervisor
        subagent2 := NewSubAgentLoop(...)
        result2 := subagent2.Run(taskWithGuidance)
    }
}
```

### Why NOT to port the full Python materialization system

1. **yaah is not containerized.** The OverlayFS path (Layer 4) is the
   paper's headline feature (134ms fork), but it requires Linux +
   namespaces. yaah runs on Windows.

2. **File delta tracking is redundant with git.** Shepherd built
   FileDelta/FileChangeset because it needs effect-stream-derived
   state (the fold invariant). yaah doesn't have that invariant — it
   has a real filesystem. Git already tracks deltas (better — it
   handles merges, branches, binary diffs).

3. **The typed effect hierarchy + reversibility tiers add complexity
   without payoff** unless you're building the full
   state-derivable-from-effects model. For yaah's use case (rollback +
   restart), you need: (a) know what changed, (b) undo it, (c) restore
   conversation state. Git + message snapshots do all three.

### What to keep from the existing port

- **Trace kernel**: Keep as-is. It's the durable record for observability.
- **Effect bus**: Keep for real-time monitoring (once sub-agents are
  properly wired to the shared store).
- **Scope lifecycle**: Keep for tracking fork/merge/discard semantics
  in the trace. But add checkpoint create/restore as new scope methods.
- **Supervisor rules engine**: Keep the rule definitions, but move
  evaluation to the pre-tool hook for synchronous interception.

### Estimated effort

| Component | LOC | Difficulty | Risk |
|-----------|-----|------------|------|
| Git-based checkpoint | ~200 | Medium | Low (git is well-understood) |
| Pre-tool interception | ~100 | Low | Low (pipeline already has hooks) |
| Loop integration | ~150 | Medium | Medium (sub-agent restart semantics) |
| Sub-agent bus wiring fix | ~30 | Low | Low |
| **Total** | **~480** | | |

Compare with porting the full Python materialization system: ~2000+
lines across FileDelta, FileChangeset, SimpleWorkspaceMaterializer,
MaterializationManager, EffectMaterializationManager, overlay
extraction — and it still wouldn't work on Windows.
