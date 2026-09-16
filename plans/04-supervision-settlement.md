# Plan 04 — Check-at-Commit Supervision & Consume-Once Settlement

**Phase**: 3 · **Priority**: P2 · **Estimate**: ~2–3 weeks (~800–1200 LOC + tests)
**Goal**: Adopt the two patterns that make Python Shepherd's supervision and
output-review story compelling, expressed on the Go port's existing
`Scope`/`Sandbox`/`Supervisor` primitives:

1. **Check-at-commit**: nothing from a child scope reaches the parent without
   passing a review gate — the Go analogue of `SubstrateOperationProposed` →
   approve-by-return / deny-by-raise (`SupervisorDenied`).
2. **Consume-once settlement**: child results are *sealed* as retained outputs
   and settled exactly once via `select / apply / release / discard` — the
   lightweight analogue of vcs-core's settlement machine.

**Python anchors**:
- `../shepherd/shepherd/packages/dialect/src/shepherd_dialect/supervision.py`
  (67 L — the pattern, not the code: proposal at the last undo point,
  `SupervisorDenied(effect, reason)`, denial recorded as a
  `supervisor.decision` trace event, `drafts_only_supervisor` as the worked
  per-path example)
- vcs-core retained-output lifecycle (`_retained_output_{selection,settlement,application}*.py`)
  and dialect settlement verbs (`select` fast-forward-only, `apply` three-way +
  path-disjoint, `release`, `discard`)
- `../shepherd/examples/workspace-handles/{best_of_n,retry_until_acceptable,apply_onto_moved_workspace}.py`
- Record vocabulary: `../shepherd2/src/shepherd2/schemas/run_outputs.py`
  (minimal subset only)

**Depends on**: nothing hard. Pairs naturally with plan 02 (executions give
outputs an identity) but works at scope level alone. Plan 01 §1 should be in
(record digests).

---

## 1. Check-at-commit merge gate (supervisor.go / scope_manager.go)

Today `ScopeManager.Merge(childID)` is ungated: it records the merge and
tears down the sandbox. Add a propose/commit split:

```go
// The proposal vocabulary
type ProposedChange struct {
    Path string          // workspace-relative
    Kind string          // "create" | "modify" | "delete"
    Diff string          // unified diff hunk for modify; content preview (truncated) for create
}
type MergeProposal struct {
    ChildScopeID string
    BaselineDigest string     // WorkspaceState digest the diff is against
    Changes      []ProposedChange
    Diff         string       // full DiffWorkspace output (truncated to maxLines)
}

// Reviewer contract: approve by returning nil, deny by returning error.
type MergeReviewer func(proposal MergeProposal) error

type SupervisorDeniedError struct { Reason string; Changes []ProposedChange }
// error text mirrors Python: denial reason included; deny → Discard, never Merge

func (m *ScopeManager) ProposeMerge(ctx context.Context, childID string, maxDiffLines int) (*MergeProposal, error)
    // requires child sandbox with Diff capability; baseline = parent's captured
    // WorkspaceState at fork time (captured automatically at Fork when the child
    // is isolated; for shared/in-place sandboxes, baseline = latest parent capture)

func (m *ScopeManager) CommitMerge(ctx context.Context, childID string, review MergeReviewer) error
    // 1. ProposeMerge
    // 2. append shepherd.merge.proposed.v1 (capture, child owner path):
    //      {child_scope, baseline_digest, change_count, changes:[{path,kind}], diff_digest}
    // 3. run review(proposal)
    // 4a. nil    → append shepherd.supervisor.decision.v1 {approved:true}  → Merge(child)
    // 4b. error  → append shepherd.supervisor.decision.v1 {approved:false, reason}
    //            → Discard(child) → return *SupervisorDeniedError
    // Plain Merge(childID) stays available for callers who deliberately skip review
    // (documented as such — parity with Python where supervision is opt-in frames).
```

Design notes:
- **Where the baseline comes from**: `ForkIsolated` already snapshots the
  child's starting point implicitly (worktree at repo HEAD / container parent
  key). Record the parent's `WorkspaceState` digest in the fork payload
  (extend `shepherd.scope.forked.v1` payload with optional `baseline_digest` —
  payload addition is backwards-compatible for readers; **new schema version
  not required** since payloads are open maps, but note it in CHANGELOG).
- Built-in reviewers mirroring Python examples:
  `DraftsOnlyReviewer(prefix string) MergeReviewer` (deny changes outside
  `prefix` — the `drafts_only_supervisor` analogue), and a
  `DestructivePathReviewer(patterns []string)` built from the existing
  `isDestructiveToolCall` vocabulary.
- Wire to `Supervisor`: add
  `func (s *Supervisor) ReviewMerge(scopeID string, proposal MergeProposal) *Intervention`
  so rule engines can produce decisions (deny → `InterventionDiscard` finally
  gets an emitter; the two currently dead intervention types gain a use).

## 2. Seal + consume-once settlement (settlement.go, new file)

```go
const (
    SchemaRunOutputSealed    = "shepherd.run_output.sealed.v1"     // declaration
    SchemaRunOutputSettled   = "shepherd.run_output.settled.v1"    // capture
)

type SettlementAction string
const (
    SettleSelect  SettlementAction = "selected"
    SettleApply   SettlementAction = "applied"
    SettleRelease SettlementAction = "released"
    SettleDiscard SettlementAction = "discarded"
)

type OutputState string
const (
    OutputUnconsumed OutputState = "unconsumed"
    OutputSelected  OutputState = "selected"
    OutputApplied   OutputState = "applied"
    OutputReleased  OutputState = "released"
    OutputDiscarded OutputState = "discarded"
    OutputInvalid   OutputState = "invalid"   // failed apply, etc.
)

type RetainedOutput struct {
    ID          string          // "out:<owner>:<seq>"
    ScopeID     string
    Workspace   WorkspaceState  // sealed sandbox state
    Changes     []ProposedChange // frozen at seal time
    Diff        string
    State       OutputState
    Action      SettlementAction // zero until settled
    SealedAt    time.Time
    SettledAt   time.Time
}

func (s *Scope) Seal(ctx context.Context) (*RetainedOutput, error)
    // Capture + Diff against baseline; append sealed.v1 {output_id, scope,
    // state_digest, change_count}; register in ScopeManager; scope stays active
    // (sealing does not merge) — parity with retained outputs outliving runs.

func (o *RetainedOutput) Settle(ctx context.Context, action SettlementAction) error
    // consume-once: any second Settle → ErrOutputConsumed
    // selected  → merge child into parent baseline (git: fast-forward-only when
    //             parent unmoved; else error with "use apply" — parity with
    //             select's fast-forward rule)
    // applied   → three-way: re-diff parent now-vs-baseline; require the parent's
    //             concurrent changes to be path-disjoint from o.Changes; apply
    //             via sandbox Apply of the sealed state onto current parent HEAD
    //             (git backend: apply as patch/stash-apply; containerd: prepare
    //             from committed key); path overlap → ErrApplyConflict, state
    //             remains unconsumed (apply failure must not consume)
    // released  → mark consumed, keep the sealed state addressable (no workspace
    //             change; parity: release = "keep, don't integrate")
    // discarded → destroy child sandbox if owned; sealed state retained as record
    // every settle appends settled.v1 {output_id, action, state_digest}
```

Registry on `ScopeManager`:

```go
func (m *ScopeManager) Seal(ctx, scopeID string) (*RetainedOutput, error)
func (m *ScopeManager) Output(id string) (*RetainedOutput, bool)
func (m *ScopeManager) OutputsForScope(scopeID string) []*RetainedOutput
func (m *ScopeManager) Settle(ctx, outputID string, action SettlementAction) error
```

State machine (mirrors Python `RunOutputState`, minus skeleton-lane states):

```
unconsumed ─select→ selected(terminal)
           ─apply→  applied(terminal)     (apply error → stays unconsumed)
           ─release→ released(terminal)
           ─discard→ discarded(terminal)
any double-settle → ErrOutputConsumed; corrupt state → invalid
```

Persistence: sealed/settled **records** live in the trace (durable). The
in-memory registry follows the same durability gap as checkpoints — plan 05
§4 recovery rebuilds `RetainedOutput` metadata from trace records (seal
payload carries state_digest + backend + revision, enough to reconstruct).

## 3. Recipes as executable docs (examples_test.go)

Port the three workspace-handle examples as Go integration tests (they are
the reference scenarios consumers will ask for):

- `TestExample_BestOfN`: fork N isolated worktree scopes from one repo, each
  "does work" (writes different files), Seal all, inspect `Changes`/`Diff`,
  settle exactly one with `SettleSelect`, `SettleDiscard` the rest; assert
  parent workspace contains only the winner; assert second settle of the
  winner fails.
- `TestExample_RetryUntilAcceptable`: loop {fork → work → checkpoint →
  ProposeMerge with `DraftsOnlyReviewer("out/")` → deny → Discard → restore →
  retry with injected guidance} until approved → CommitMerge. Shows
  CheckCall (pre-tool deny) and merge review (commit deny) in one flow.
- `TestExample_ApplyOntoMovedWorkspace`: seal child output; meanwhile parent
  workspace advances with **disjoint** paths → `SettleApply` succeeds; repeat
  with overlapping path → `ErrApplyConflict`, output still unconsumed.

## 4. Non-goals

- No vcs-core world values, sealing handoffs (`SealCandidateHandoff`),
  citations/locators/descriptor-locator vocabulary, publication plans,
  custody validation.
- No `Changeset` view type beyond `ProposedChange` (yaah already renders diffs).
- No async/queue settlement; `Settle` is synchronous.
- Full `run_outputs.py` schema fidelity: we intentionally define
  `shepherd.run_output.sealed.v1`/`settled.v1` (new refs, documented as
  Go-kernel extensions like the existing scope/checkpoint schemas — **not**
  pretending to be `shepherd2.run_output.v1`). If cross-language output
  readers ever matter, revisit with the full descriptor vocabulary.

## 5. Acceptance criteria

- [ ] `CommitMerge` with a denying reviewer: parent workspace untouched,
      child discarded, `supervisor.decision.v1` (denied) in trace, error type
      carries reason.
- [ ] Approving reviewer merges and records both proposed + decision records.
- [ ] Settlement verbs enforce consume-once; apply-conflict leaves output
      unconsumed; select refuses non-fast-forward with actionable error.
- [ ] All three recipe tests green on Windows (git worktree carrier) — no
      Linux-only assumptions.
- [ ] `InterventionDiscard` has a live emitter (supervisor-driven denial).
- [ ] README: "Review before merge / settle outputs" section with the
      best-of-n snippet.
