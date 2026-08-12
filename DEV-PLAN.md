# Development Plan: Checkpoint/Restore + Synchronous Interception

**Project**: shepherd-kernel-go
**Goal**: Enable "subagent makes a change, supervisor rolls it back and restarts from an earlier point" — entirely within shepherd-kernel-go, no yaah changes required.

---

## Design Principles

1. **shepherd-kernel-go stays a pure library** — no yaah imports, no agent loop knowledge
2. **Git is the materializer** — no custom FileDelta/changeset/overlay code. `git add -A && git stash create` captures state; `git reset --hard + git clean -fd + git stash apply` restores it.
3. **Opaque snapshots** — the caller (yaah, later) passes conversation state as `[]byte`. shepherd-kernel-go stores it, returns it on restore, never inspects it.
4. **Same test patterns as existing code** — `newMemStore(t)`, trace verification via `ReadOwnerPrefix`, table-driven tests.
5. **Build on what exists** — add methods to `Scope` and `Supervisor`, don't restructure.

---

## Phase 1: Git Workspace Checkpoint

**File**: `checkpoint.go` (~200 lines)
**Test**: `checkpoint_test.go` (~250 lines)

### 1.1 Types

```go
// checkpoint.go

// Schema refs for trace recording.
const (
    SchemaCheckpointCreated  = "shepherd.checkpoint.created.v1"
    SchemaCheckpointRestored = "shepherd.checkpoint.restored.v1"
)

// CheckpointState tracks whether a checkpoint is usable.
type CheckpointState string

const (
    CheckpointValid   CheckpointState = "valid"
    CheckpointUsed    CheckpointState = "used"     // restored already
    CheckpointStale   CheckpointState = "stale"    // HEAD moved underneath us
    CheckpointInvalid CheckpointState = "invalid"  // git operations failed
)

// GitCheckpoint captures workspace + conversation state at a point in time.
//
// The workspace state is a git stash SHA (or HEAD ref if the tree was clean).
// The conversation state is an opaque []byte the caller provides — typically
// JSON-marshaled []types.Message from the agent loop.
//
// Checkpoints are single-use: RestoreCheckpoint marks them as "used".
// A stale checkpoint (HEAD moved after creation without a restore) is detected
// via HEAD comparison.
type GitCheckpoint struct {
    // ID is a unique checkpoint identifier (auto-generated).
    ID string
    // ScopeID is the scope this checkpoint belongs to.
    ScopeID string
    // StashSHA is the git stash commit SHA. Empty if the tree was clean
    // at checkpoint time (HeadSHA captures the state in that case).
    StashSHA string
    // HeadSHA is the git HEAD at checkpoint time. Used for staleness detection.
    HeadSHA string
    // Snapshot is the opaque caller-provided conversation state.
    Snapshot []byte
    // CreatedAt is when the checkpoint was taken.
    CreatedAt time.Time
    // State tracks usability.
    State CheckpointState
}
```

### 1.2 Git Operations

All git commands run via `os/exec` with the repo path as working directory. Each command is a separate function for testability.

```go
// checkpoint.go

// gitRunner executes git commands in a repo directory.
// This is a struct (not bare functions) so tests can swap in a fake
// runner via an interface, avoiding the need for real git in unit tests
// that only test trace recording / state management.
type gitRunner struct {
    repoPath string
}

// run executes a git command and returns trimmed stdout.
func (g *gitRunner) run(args ...string) (string, error) {
    cmd := exec.Command("git", args...)
    cmd.Dir = g.repoPath
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    if err := cmd.Run(); err != nil {
        return "", fmt.Errorf("git %s: %w (stderr: %s)",
            strings.Join(args, " "), err, stderr.String())
    }
    return strings.TrimSpace(stdout.String()), nil
}

// stageAll runs `git add -A` to capture untracked files.
func (g *gitRunner) stageAll() error {
    _, err := g.run("add", "-A")
    return err
}

// stashCreate runs `git stash create` and returns the stash commit SHA.
// Returns empty string if the working tree is clean (no changes to stash).
func (g *gitRunner) stashCreate() (string, error) {
    return g.run("stash", "create")
}

// headSHA returns the current HEAD commit SHA.
func (g *gitRunner) headSHA() (string, error) {
    return g.run("rev-parse", "HEAD")
}

// resetHard runs `git reset --hard` — discards all tracked file changes.
func (g *gitRunner) resetHard() error {
    _, err := g.run("reset", "--hard")
    return err
}

// cleanFiles runs `git clean -fd` — removes untracked files and directories.
func (g *gitRunner) cleanFiles() error {
    _, err := g.run("clean", "-fd")
    return err
}

// stashApply runs `git stash apply <sha>` — restores stashed changes.
func (g *gitRunner) stashApply(sha string) error {
    _, err := g.run("stash", "apply", sha)
    return err
}

// isRepo returns true if repoPath is inside a git working tree.
func (g *gitRunner) isRepo() bool {
    _, err := g.run("rev-parse", "--is-inside-work-tree")
    return err == nil
}
```

### 1.3 CreateCheckpoint

```go
// checkpoint.go

// CreateCheckpoint captures the current workspace state and caller snapshot.
//
// Workspace state is captured via git:
//   1. `git add -A` — stage everything (including untracked files)
//   2. `git stash create` — create a stash commit (returns SHA, or empty if clean)
//   3. `git rev-parse HEAD` — record HEAD for staleness detection
//
// The stash is NOT popped — the working tree is unchanged after checkpoint.
// The stash SHA lets us restore to this exact state later.
//
// Records a "checkpoint.created" declaration in the scope's trace.
func (s *Scope) CreateCheckpoint(repoPath string, snapshot []byte) (*GitCheckpoint, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    if s.state != ScopeActive {
        return nil, fmt.Errorf("cannot checkpoint scope %s in state %s", s.id, s.state)
    }

    g := &gitRunner{repoPath: repoPath}

    if !g.isRepo() {
        return nil, fmt.Errorf("checkpoint: %s is not a git repository", repoPath)
    }

    if err := g.stageAll(); err != nil {
        return nil, fmt.Errorf("checkpoint: git add -A: %w", err)
    }

    stashSHA, err := g.stashCreate()
    if err != nil {
        return nil, fmt.Errorf("checkpoint: git stash create: %w", err)
    }

    headSHA, err := g.headSHA()
    if err != nil {
        return nil, fmt.Errorf("checkpoint: git rev-parse HEAD: %w", err)
    }

    cp := &GitCheckpoint{
        ID:        fmt.Sprintf("cp:%s:%d", s.ownerID, time.Now().UnixNano()),
        ScopeID:   s.id,
        StashSHA:  stashSHA,
        HeadSHA:   headSHA,
        Snapshot:  snapshot,
        CreatedAt: time.Now(),
        State:     CheckpointValid,
    }

    // Record in trace.
    payload := map[string]any{
        "checkpoint_id": cp.ID,
        "stash_sha":     stashSHA,
        "head_sha":      headSHA,
        "has_snapshot":  len(snapshot) > 0,
    }
    _, err = s.store.Append(TrustedAppendContext, AppendBatch{
        AppendIntentID: fmt.Sprintf("%s:checkpoint:%d", s.ownerID, time.Now().UnixNano()),
        Groups: []AppendGroup{{
            TraceOwnerID: s.ownerID,
            FactDrafts: []RecordDraft{{
                Mode:      Declaration,
                SchemaRef: SchemaCheckpointCreated,
                KindLabel: "checkpoint:created",
                Payload:   payload,
            }},
        }},
    })
    if err != nil {
        return nil, fmt.Errorf("checkpoint: record in trace: %w", err)
    }

    return cp, nil
}
```

### 1.4 RestoreCheckpoint

```go
// checkpoint.go

// RestoreCheckpoint reverts the workspace to the checkpointed state and
// returns the stored conversation snapshot.
//
// Workspace restore via git:
//   1. `git reset --hard` — discard all tracked changes since checkpoint
//   2. `git clean -fd` — remove untracked files/dirs created since checkpoint
//   3. `git stash apply <sha>` — re-apply the checkpointed changes (if any)
//
// The snapshot (opaque []byte) is returned for the caller to deserialize
// and restore to its agent loop's context manager.
//
// Records a "checkpoint.restored" capture in the scope's trace.
// Marks the checkpoint as used — calling Restore twice returns an error.
func (s *Scope) RestoreCheckpoint(cp *GitCheckpoint) ([]byte, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    if s.state != ScopeActive {
        return nil, fmt.Errorf("cannot restore in scope %s state %s", s.id, s.state)
    }

    if cp.ScopeID != s.id {
        return nil, fmt.Errorf("checkpoint %s belongs to scope %s, not %s",
            cp.ID, cp.ScopeID, s.id)
    }

    if cp.State != CheckpointValid {
        return nil, fmt.Errorf("checkpoint %s is %s, cannot restore", cp.ID, cp.State)
    }

    g := &gitRunner{repoPath: repoPathFromScope(s)} // See note below
    // Actually: we need repoPath. Store it on the checkpoint at creation.
    // Add RepoPath field to GitCheckpoint (set by CreateCheckpoint).

    // Step 1: Wipe workspace back to HEAD
    if err := g.resetHard(); err != nil {
        cp.State = CheckpointInvalid
        return nil, fmt.Errorf("restore: git reset --hard: %w", err)
    }

    // Step 2: Remove untracked files
    if err := g.cleanFiles(); err != nil {
        cp.State = CheckpointInvalid
        return nil, fmt.Errorf("restore: git clean -fd: %w", err)
    }

    // Step 3: Re-apply checkpointed changes (if any)
    if cp.StashSHA != "" {
        if err := g.stashApply(cp.StashSHA); err != nil {
            cp.State = CheckpointInvalid
            return nil, fmt.Errorf("restore: git stash apply %s: %w", cp.StashSHA, err)
        }
    }

    // Step 4: Staleness check — HEAD should not have moved
    currentHead, _ := g.headSHA()
    if currentHead != cp.HeadSHA {
        // HEAD moved — we're on a different commit base.
        // Log a warning but don't fail — the stash apply still worked,
        // it just may produce conflicts on a different base.
        slog.Warn("checkpoint restore: HEAD moved since checkpoint",
            "expected", cp.HeadSHA, "actual", currentHead)
    }

    cp.State = CheckpointUsed

    // Record in trace.
    _, err := s.store.Append(TrustedAppendContext, AppendBatch{
        AppendIntentID: fmt.Sprintf("%s:restore:%d", s.ownerID, time.Now().UnixNano()),
        Groups: []AppendGroup{{
            TraceOwnerID: s.ownerID,
            FactDrafts: []RecordDraft{{
                Mode:      Capture,
                SchemaRef: SchemaCheckpointRestored,
                KindLabel: "checkpoint:restored",
                Payload: map[string]any{
                    "checkpoint_id": cp.ID,
                    "head_sha":      currentHead,
                },
            }},
        }},
    })
    if err != nil {
        slog.Debug("checkpoint: record restore in trace failed", "err", err)
    }

    return cp.Snapshot, nil
}
```

**Note on RepoPath**: Add `RepoPath string` to `GitCheckpoint`, set by `CreateCheckpoint`. This way `RestoreCheckpoint` doesn't need a repoPath argument — it's stored on the checkpoint.

### 1.5 Test Plan

```go
// checkpoint_test.go

// Helper: create a temp git repo with an initial commit.
func newTestRepo(t *testing.T) string {
    t.Helper()
    dir := t.TempDir()
    mustGit(t, dir, "init")
    mustGit(t, dir, "config", "user.email", "test@test.com")
    mustGit(t, dir, "config", "user.name", "Test")
    os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0644)
    mustGit(t, dir, "add", "-A")
    mustGit(t, dir, "commit", "-m", "init")
    return dir
}

// Helper: run git in a dir, fail test on error.
func mustGit(t *testing.T, dir string, args ...string) { ... }
```

**Test cases:**

| Test | Description |
|------|-------------|
| `TestCheckpoint_CreateCleanRepo` | Checkpoint on clean repo → StashSHA empty, HeadSHA set, State valid |
| `TestCheckpoint_CreateDirtyRepo` | Modify a file → checkpoint → StashSHA non-empty |
| `TestCheckpoint_CreateWithUntracked` | Add untracked file → checkpoint → file is captured |
| `TestCheckpoint_CreateRecordsInTrace` | Verify "checkpoint.created" declaration in trace |
| `TestCheckpoint_RestoreRevertsModifications` | Checkpoint → modify file → restore → file is back |
| `TestCheckpoint_RestoreRevertsUntracked` | Checkpoint → create new file → restore → file is gone |
| `TestCheckpoint_RestoreRevertsDeletions` | Checkpoint → delete file → restore → file is back |
| `TestCheckpoint_RestoreReturnsSnapshot` | Snapshot bytes round-trip through restore |
| `TestCheckpoint_RestoreRecordsInTrace` | Verify "checkpoint.restored" capture in trace |
| `TestCheckpoint_RestoreTwiceFails` | Restore once → State=used → second restore errors |
| `TestCheckpoint_RestoreNonGitRepo` | Non-repo path → CreateCheckpoint errors |
| `TestCheckpoint_RestoreWrongScope` | Checkpoint from scope A, restore on scope B → errors |
| `TestCheckpoint_NotOnActiveScope` | Halt scope → CreateCheckpoint/Restore errors |
| `TestCheckpoint_StashApplyConflict` | Modify same file differently post-checkpoint → restore handles gracefully |

### 1.6 Scope struct change

Add `RepoPath` to `GitCheckpoint` (not to `Scope` — the scope doesn't need to know about the repo path permanently; it's stored per-checkpoint):

```go
// In checkpoint.go, GitCheckpoint gets:
type GitCheckpoint struct {
    // ... existing fields ...
    RepoPath string  // set by CreateCheckpoint, used by RestoreCheckpoint
}
```

---

## Phase 2: Synchronous Supervisor Interception

**File**: `supervisor.go` (modify — add ~60 lines)
**Test**: `supervisor_test.go` (modify — add ~80 lines)

### 2.1 New Intervention Type

```go
// supervisor.go — add to InterventionType constants

const (
    InterventionInject  InterventionType = "inject"
    InterventionFork    InterventionType = "fork"
    InterventionDiscard InterventionType = "discard"
    InterventionHalt    InterventionType = "halt"
    InterventionDeny    InterventionType = "deny"   // NEW: block the tool call
)
```

### 2.2 CheckCall Method

The existing `evaluate()` method runs in a background goroutine reading
from the bus. `CheckCall` is a synchronous entry point that evaluates the
same rules inline. This is what a pre-tool hook would call.

```go
// supervisor.go

// CheckCall evaluates rules synchronously against a tool-call event.
// Returns nil if the call is approved (no rule matched or all rules
// returned nil). Returns a non-nil Intervention if a rule wants to
// block or modify the call.
//
// This is the synchronous counterpart to the async bus loop. The caller
// (agent loop, pre-tool hook) calls this BEFORE executing the tool and
// checks the result:
//   - nil → proceed with the tool call
//   - InterventionDeny → block the tool call, return the reason to the model
//   - InterventionInject → proceed but inject guidance into the next turn
//   - InterventionHalt → stop the sub-agent entirely
func (s *Supervisor) CheckCall(event EffectEvent) *Intervention {
    s.mu.RLock()
    rules := make([]SupervisionRule, len(s.rules))
    copy(rules, s.rules)
    s.mu.RUnlock()

    for _, rule := range rules {
        if !rule.Match(event) {
            continue
        }
        iv := rule.Action(event)
        if iv != nil {
            return iv
        }
    }
    return nil
}
```

Note: This is the same evaluation logic as `evaluate()` but synchronous
and returning the first intervention directly (instead of sending to a
channel). The `evaluate()` method already has "first match wins"
semantics — `CheckCall` matches that exactly.

### 2.3 DestructiveToolRule upgrade

The existing `DestructiveToolRule` returns `InterventionInject` (a
warning). For synchronous interception, we need a variant that returns
`InterventionDeny` to actually block. Add a constructor parameter:

```go
// supervisor.go

// DestructiveToolGuard returns a rule that DENIES destructive tool calls
// (blocking them before execution). This is the synchronous variant of
// DestructiveToolRule — use in CheckCall context.
func DestructiveToolGuard() SupervisionRule {
    return SupervisionRule{
        Name: "destructive_file_block",
        Match: func(e EffectEvent) bool {
            if e.Mode != Declaration {
                return false
            }
            return isDestructiveToolCall(e)  // reuse existing helper
        },
        Action: func(e EffectEvent) *Intervention {
            return &Intervention{
                Type:    InterventionDeny,
                ScopeID: fmt.Sprintf("scope:%s", e.TraceOwnerID),
                Payload: fmt.Sprintf("Blocked destructive operation (%s). Supervisor denied.", e.KindLabel),
                Time:    time.Now(),
            }
        },
    }
}
```

### 2.4 Test Plan

| Test | Description |
|------|-------------|
| `TestSupervisor_CheckCall_Approved` | Non-matching event → nil (approved) |
| `TestSupervisor_CheckCall_Deny` | Matching destructive event → InterventionDeny |
| `TestSupervisor_CheckCall_FirstRuleWins` | Two matching rules → first one's intervention returned |
| `TestSupervisor_CheckCall_ObserveOnly` | Rule matches but Action returns nil → nil (approved) |
| `TestDestructiveToolGuard_BashRm` | rm command → InterventionDeny |
| `TestDestructiveToolGuard_BashSafe` | ls command → no match |
| `TestDestructiveToolGuard_Write` | write tool → InterventionDeny |

---

## Phase 3: Scope Checkpoint Registry

**File**: `scope_manager.go` (modify — add ~50 lines)
**Test**: `scope_test.go` (modify — add ~40 lines)

### 3.1 ScopeManager Checkpoint Tracking

```go
// scope_manager.go — add to ScopeManager struct

type ScopeManager struct {
    store       *SQLiteTraceStore
    scopes      map[string]*Scope
    checkpoints map[string]*GitCheckpoint  // NEW: checkpoint ID → checkpoint
    mu          sync.RWMutex
}

// NewScopeManager updated:
func NewScopeManager(store *SQLiteTraceStore) *ScopeManager {
    return &ScopeManager{
        store:       store,
        scopes:      make(map[string]*Scope),
        checkpoints: make(map[string]*GitCheckpoint),
    }
}
```

### 3.2 Checkpoint Registry Methods

```go
// scope_manager.go

// CreateCheckpoint creates a checkpoint for a scope and registers it.
func (m *ScopeManager) CreateCheckpoint(scopeID, repoPath string, snapshot []byte) (*GitCheckpoint, error) {
    m.mu.Lock()
    defer m.mu.Unlock()

    scope, ok := m.scopes[scopeID]
    if !ok {
        return nil, fmt.Errorf("scope %s not found", scopeID)
    }

    cp, err := scope.CreateCheckpoint(repoPath, snapshot)
    if err != nil {
        return nil, err
    }

    m.checkpoints[cp.ID] = cp
    return cp, nil
}

// RestoreCheckpoint restores a registered checkpoint and returns the snapshot.
func (m *ScopeManager) RestoreCheckpoint(checkpointID string) ([]byte, error) {
    m.mu.RLock()
    cp, ok := m.checkpoints[checkpointID]
    m.mu.RUnlock()

    if !ok {
        return nil, fmt.Errorf("checkpoint %s not found", checkpointID)
    }

    scope, ok := m.scopes[cp.ScopeID]
    if !ok {
        return nil, fmt.Errorf("scope %s for checkpoint %s not found", cp.ScopeID, checkpointID)
    }

    return scope.RestoreCheckpoint(cp)
}

// LatestCheckpoint returns the most recent checkpoint for a scope, or nil.
func (m *ScopeManager) LatestCheckpoint(scopeID string) *GitCheckpoint {
    m.mu.RLock()
    defer m.mu.RUnlock()

    var latest *GitCheckpoint
    for _, cp := range m.checkpoints {
        if cp.ScopeID == scopeID {
            if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
                latest = cp
            }
        }
    }
    return latest
}
```

### 3.3 Test Plan

| Test | Description |
|------|-------------|
| `TestScopeManager_CreateCheckpoint` | Create via manager → registered, retrievable |
| `TestScopeManager_RestoreCheckpoint` | Create → restore via manager → snapshot returned |
| `TestScopeManager_LatestCheckpoint` | Multiple checkpoints → latest by CreatedAt |
| `TestScopeManager_CheckpointScopeNotFound` | Nonexistent scope → error |

---

## Phase 4: Integration Test — Full Rollback Scenario

**File**: `integration_test.go` (~150 lines)

This test exercises the full rollback flow end-to-end within shepherd-kernel-go, simulating what yaah would do:

```go
// integration_test.go

func TestIntegration_RollbackScenario(t *testing.T) {
    // 1. Set up: trace store, bus, scope manager, supervisor
    store := newMemStore(t)
    bus := NewEffectBus(64)
    defer bus.Close()
    store.WithBus(bus)
    mgr := NewScopeManager(store)

    // 2. Create a git repo with initial content
    repo := newTestRepo(t)
    os.WriteFile(filepath.Join(repo, "main.go"),
        []byte("package main\n\nfunc main() {}\n"), 0644)
    mustGit(t, repo, "add", "-A")
    mustGit(t, repo, "commit", "-m", "initial code")

    // 3. Create a scope for the sub-agent
    scope, _ := mgr.Create("sub:worker")

    // 4. Take a checkpoint BEFORE the sub-agent runs
    conversationSnapshot := []byte(`{"messages":["system prompt","task: fix the bug"]}`)
    cp, err := mgr.CreateCheckpoint(scope.ID(), repo, conversationSnapshot)
    // assert no error, cp.State == CheckpointValid

    // 5. Simulate the sub-agent making changes (destructively)
    os.WriteFile(filepath.Join(repo, "main.go"),
        []byte("BROKEN CODE THAT SHOULD BE REVERTED"), 0644)
    os.WriteFile(filepath.Join(repo, "new_file.go"),
        []byte("UNWANTED NEW FILE"), 0644)
    os.Remove(filepath.Join(repo, "README.md"))

    // 6. Simulate the supervisor deciding to roll back
    snapshot, err := mgr.RestoreCheckpoint(cp.ID)
    // assert no error

    // 7. Verify workspace is restored
    mainGo, _ := os.ReadFile(filepath.Join(repo, "main.go"))
    // assert mainGo contains "package main" (original content)

    _, err = os.Stat(filepath.Join(repo, "new_file.go"))
    // assert os.IsNotExist(err) — untracked file was cleaned

    readme, _ := os.ReadFile(filepath.Join(repo, "README.md"))
    // assert readme contains "# Test" — deleted file was restored

    // 8. Verify conversation snapshot round-tripped
    // assert snapshot == conversationSnapshot

    // 9. Verify trace records both checkpoint.create and checkpoint.restore
    slice, _ := store.ReadOwnerPrefix(TrustedReadContext, "sub:worker", 99, ModeBoth)
    // Find SchemaCheckpointCreated (Declaration)
    // Find SchemaCheckpointRestored (Capture)
}
```

Additional integration tests:

| Test | Description |
|------|-------------|
| `TestIntegration_SynchronousDeny` | Supervisor with DestructiveToolGuard → CheckCall → InterventionDeny |
| `TestIntegration_CheckpointForkRestore` | Fork scope → checkpoint child → work → restore child → work again |
| `TestIntegration_MultipleCheckpoints` | Create cp1 → work → create cp2 → work → restore cp2 → restore cp1 |

---

## File Inventory

| File | Status | LOC (est.) | Description |
|------|--------|------------|-------------|
| `checkpoint.go` | NEW | ~200 | GitCheckpoint, gitRunner, CreateCheckpoint, RestoreCheckpoint |
| `checkpoint_test.go` | NEW | ~250 | 14 test cases for checkpoint create/restore |
| `integration_test.go` | NEW | ~150 | End-to-end rollback + synchronous deny scenarios |
| `supervisor.go` | MODIFY | +60 | CheckCall method, InterventionDeny, DestructiveToolGuard |
| `supervisor_test.go` | MODIFY | +80 | 7 test cases for CheckCall + guard |
| `scope_manager.go` | MODIFY | +50 | Checkpoint registry (CreateCheckpoint, RestoreCheckpoint, LatestCheckpoint) |
| `scope_test.go` | MODIFY | +40 | 4 test cases for manager checkpoint methods |
| **Total** | | **~830** | |

---

## Execution Order

```
Phase 1: checkpoint.go + checkpoint_test.go
  ├── Define GitCheckpoint, gitRunner
  ├── Implement CreateCheckpoint on Scope
  ├── Implement RestoreCheckpoint on Scope
  ├── Write 14 checkpoint tests (temp git repos)
  └── Run: go test -run TestCheckpoint -v

Phase 2: supervisor.go + supervisor_test.go
  ├── Add InterventionDeny constant
  ├── Add CheckCall method
  ├── Add DestructiveToolGuard constructor
  ├── Write 7 CheckCall tests
  └── Run: go test -run 'TestSupervisor_CheckCall|TestDestructiveToolGuard' -v

Phase 3: scope_manager.go + scope_test.go
  ├── Add checkpoints map to ScopeManager
  ├── Add CreateCheckpoint, RestoreCheckpoint, LatestCheckpoint
  ├── Update NewScopeManager to init checkpoints map
  ├── Write 4 manager checkpoint tests
  └── Run: go test -run 'TestScopeManager_.*Checkpoint' -v

Phase 4: integration_test.go
  ├── Write full rollback scenario test
  ├── Write synchronous deny scenario test
  ├── Write fork/checkpoint/restore test
  ├── Run: go test -run TestIntegration -v
  └── Run full suite: go test ./... -count=1
```

---

## Limitations & Explicit Non-Goals

### What this does NOT roll back

- **Network side effects** — HTTP calls, API requests, emails. These are irreversible. The trace records them (audit), but git can't undo them. Same limitation as Python Shepherd's `reversibility = NONE` tier.
- **Database writes** — Not git-tracked. Would need compensation handlers (Python's `COMPENSABLE` tier). Out of scope.
- **Compiled artifacts outside the repo** — Binaries installed to system paths, global config changes.

### What this does NOT implement (deferred to yaah integration)

- **Pre-tool hook** — The `CheckCall` method exists, but wiring it into yaah's tool dispatch path is a yaah change (~5 lines).
- **Conversation snapshot marshaling** — shepherd-kernel-go takes `[]byte`. yaah marshals `[]types.Message` to JSON. That's a yaah change.
- **Sub-agent restart loop** — The "restart from checkpoint" orchestration logic. yaah change.

### Why not port Python's reversibility tiers

The Python tier system (AUTO/COMPENSABLE/NONE) drives the materialization ordering: AUTO first, then COMPENSABLE, then NONE. If a non-reversible context fails, rollback the reversible ones.

In the Go port with git-only materialization, all filesystem effects are effectively AUTO (git handles them uniformly). Network/DB effects are NONE (no rollback). There's no COMPENSABLE tier. The tier system adds complexity without payoff unless you build multiple materializers, which the gap report argued against.

---

## Dependencies

- **No new Go dependencies.** Everything uses stdlib (`os`, `os/exec`, `fmt`, `time`, `bytes`, `strings`, `log/slog`).
- **External dependency**: `git` binary on `$PATH`. Already a safe assumption for a code agent harness.
- **Go version**: 1.21+ (matches existing go.mod). Uses `slog` (1.21+).
