package shepherd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Settlement schema refs for trace recording (plan 04 §2). These are
// Go-kernel extensions like the scope/checkpoint schemas — documented as
// such, not pretending to be shepherd2.run_output.v1.
const (
	// SchemaRunOutputSealed is recorded on the sealing scope's owner path
	// when an output is sealed. Declaration: the sealing is stated intent
	// that the settlement records then observe.
	SchemaRunOutputSealed = "shepherd.run_output.sealed.v1"
	// SchemaRunOutputSettled is recorded on the same owner path when the
	// output is settled, citing the sealed record.
	SchemaRunOutputSettled = "shepherd.run_output.settled.v1"
)

// SettlementAction is how a sealed output is consumed exactly once.
type SettlementAction string

const (
	SettleSelect  SettlementAction = "selected"
	SettleApply   SettlementAction = "applied"
	SettleRelease SettlementAction = "released"
	SettleDiscard SettlementAction = "discarded"
)

// OutputState is the consume-once lifecycle state of a retained output.
type OutputState string

const (
	OutputUnconsumed OutputState = "unconsumed"
	OutputSelected   OutputState = "selected"
	OutputApplied    OutputState = "applied"
	OutputReleased   OutputState = "released"
	OutputDiscarded  OutputState = "discarded"
	// OutputInvalid marks an output whose sealed state was discovered
	// unusable. No core path sets it today; it exists so a caller (or a
	// future recovery pass, plan 05 §4) can mark a state that must never be
	// settled without pretending it was consumed.
	OutputInvalid OutputState = "invalid"
)

// ErrOutputConsumed reports a second settlement of an already-settled
// output. Settlement is consume-once: every action is terminal.
var ErrOutputConsumed = errors.New("shepherd: retained output already settled")

// ErrApplyConflict reports a SettleApply whose changes overlap paths the
// parent changed since the fork baseline. The output is NOT consumed by a
// conflict — apply failure never consumes.
var ErrApplyConflict = errors.New("shepherd: apply conflict")

// RetainedOutput is a sealed child result: the child's captured workspace
// frozen at seal time, settle-able exactly once. It outlives the scope that
// sealed it — sealing does not merge — so a supervisor can compare sealed
// outputs and choose, the fork-and-choose rhythm.
//
// All fields are unexported behind lock-taking getters: Settle mutates the
// lifecycle fields under mu, and exported fields would let any caller race
// that write.
type RetainedOutput struct {
	// mu guards the lifecycle fields (state, action, settledAt) across
	// concurrent settle attempts: the whole validate → act → mark sequence
	// holds it, so two settles cannot both claim the unconsumed state.
	mu sync.Mutex

	id        string
	scopeID   string
	workspace WorkspaceState
	// baseline is the parent's fork-time state the changes are against.
	baseline WorkspaceState
	changes  []ProposedChange
	diff     string
	state    OutputState
	// action is the settlement that consumed the output; zero until settled.
	action    SettlementAction
	sealedAt  time.Time
	settledAt time.Time

	// mgr is set when the output is registered with a ScopeManager, which is
	// what makes Settle usable. Scope.Seal alone does not register.
	mgr *ScopeManager
	// sealedFactID cites the sealed record from the settled record.
	sealedFactID string
}

// ID returns the output's unique identifier ("out:<owner>:<seq>").
func (o *RetainedOutput) ID() string { return o.id }

// ScopeID returns the scope that sealed this output.
func (o *RetainedOutput) ScopeID() string { return o.scopeID }

// Workspace returns the sealed sandbox state.
func (o *RetainedOutput) Workspace() WorkspaceState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.workspace
}

// Baseline returns the parent's fork-time state the sealed changes are
// against.
func (o *RetainedOutput) Baseline() WorkspaceState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.baseline
}

// Changes returns the sealed output's proposed changes, frozen at seal time.
func (o *RetainedOutput) Changes() []ProposedChange {
	o.mu.Lock()
	defer o.mu.Unlock()
	// Copy: the caller must not be able to mutate the frozen record.
	cp := make([]ProposedChange, len(o.changes))
	copy(cp, o.changes)
	return cp
}

// Diff returns the sealed output's full diff against the baseline.
func (o *RetainedOutput) Diff() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.diff
}

// State returns the consume-once lifecycle state.
func (o *RetainedOutput) State() OutputState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state
}

// Action returns the settlement that consumed the output; the zero value
// until settled.
func (o *RetainedOutput) Action() SettlementAction {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.action
}

// SealedAt returns when the output was sealed.
func (o *RetainedOutput) SealedAt() time.Time { return o.sealedAt }

// SettledAt returns when the output was settled; the zero time until then.
func (o *RetainedOutput) SettledAt() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settledAt
}

// Settle consumes the output with the given action. It requires the output
// to be registered with a ScopeManager (ScopeManager.Seal registers;
// Scope.Seal alone does not) — use the manager's Settle directly otherwise.
func (o *RetainedOutput) Settle(ctx context.Context, action SettlementAction) error {
	if o.mgr == nil {
		return fmt.Errorf("retained output %s is not registered with a scope manager; settle through the manager that sealed it", o.id)
	}
	return o.mgr.Settle(ctx, o.id, action)
}

// Seal captures the scope's workspace as a retained output and freezes its
// changes against the fork baseline. The scope stays active — sealing does
// not merge, and the output outlives the scope by design.
//
// The scope must have a sandbox and a fork baseline (a parent with a sandbox
// at fork time); a scope without a baseline has nothing to diff against and
// sealing it would freeze changes with no base to settle them onto.
//
// Records shepherd.run_output.sealed.v1 on the scope's owner path. The append
// is hard, not advisory: the sealed record is the durable half of the output
// (plan 05 §4 rebuilds metadata from it), so a trace that cannot carry it
// must not hand out an output whose settlement would be unrecordable.
func (s *Scope) Seal(ctx context.Context) (*RetainedOutput, error) {
	s.mu.RLock()
	state := s.state
	sb := s.sandbox
	ownerID := s.ownerID
	scopeID := s.id
	baseline := s.baseline
	s.mu.RUnlock()

	if state != ScopeActive {
		return nil, fmt.Errorf("cannot seal scope %s in state %s", scopeID, state)
	}
	if sb == nil {
		return nil, ErrNoSandbox
	}
	if baseline.Backend == "" {
		return nil, fmt.Errorf("scope %s has no fork baseline: it was forked from a parent without a sandbox, so its changes have no base to settle onto", scopeID)
	}

	ws, err := sb.Capture(ctx)
	if err != nil {
		return nil, fmt.Errorf("seal scope %s: capture workspace: %w", scopeID, err)
	}

	diff, files, err := sb.Diff(ctx, baseline, 0)
	if err != nil {
		return nil, fmt.Errorf("seal scope %s: diff against baseline: %w", scopeID, err)
	}

	seq := nextCheckpointSeq.Add(1)
	out := &RetainedOutput{
		id:        fmt.Sprintf("out:%s:%d", ownerID, seq),
		scopeID:   scopeID,
		workspace: ws,
		baseline:  baseline,
		changes:   classifyChanges(diff, files),
		diff:      diff,
		state:     OutputUnconsumed,
		sealedAt:  time.Now(),
	}

	stateDigest, _ := ws.Digest()
	baselineDigest, _ := baseline.Digest()
	receipt, err := s.store.Append(ctx, TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:seal:%d", ownerID, seq),
		Groups: []AppendGroup{{
			TraceOwnerID: ownerID,
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: SchemaRunOutputSealed,
				KindLabel: "run_output:sealed",
				Payload: map[string]any{
					"output_id":       out.id,
					"scope":           scopeID,
					"backend":         ws.Backend,
					"revision":        ws.Revision,
					"state_digest":    stateDigest,
					"baseline_digest": baselineDigest,
					"change_count":    len(out.changes),
					// The full states and the change list ride in the payload so
					// recovery (plan 05 section 4) rebuilds settle-able outputs:
					// digests alone cannot restore a WorkspaceState, and the
					// overlap guard needs the paths.
					"state": map[string]any{
						"backend":  ws.Backend,
						"revision": ws.Revision,
						"data":     ws.Data,
					},
					"baseline": map[string]any{
						"backend":  baseline.Backend,
						"revision": baseline.Revision,
						"data":     baseline.Data,
					},
					"changes": changesPayload(out.changes),
				},
			}},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("seal scope %s: record sealed output: %w", scopeID, err)
	}
	if len(receipt.FactIDs) > 0 {
		out.sealedFactID = receipt.FactIDs[0]
	}
	return out, nil
}

// changesPayload renders a change list as trace payload entries.
func changesPayload(changes []ProposedChange) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, map[string]any{"path": c.Path, "kind": c.Kind})
	}
	return out
}

// stateFor maps a settlement action to its terminal output state.
func stateFor(action SettlementAction) (OutputState, error) {
	switch action {
	case SettleSelect:
		return OutputSelected, nil
	case SettleApply:
		return OutputApplied, nil
	case SettleRelease:
		return OutputReleased, nil
	case SettleDiscard:
		return OutputDiscarded, nil
	default:
		return "", fmt.Errorf("unknown settlement action %q", action)
	}
}

// Settle consumes a registered output exactly once.
//
// The verbs (plan 04 §2, mirroring the reference settlement machine):
//
//   - selected: the parent's workspace must be bit-identical to the fork
//     baseline (fast-forward-only, strictly). The sealed state is applied
//     onto the parent and the child is causally merged. A parent that moved
//     — or never had a clean tree at the fork point — gets an actionable
//     error pointing at apply.
//   - applied: three-way. The parent's changes since the baseline must be
//     path-disjoint from the output's changes; the sealed delta is then
//     merged onto the parent's current workspace (DeltaApplier). A path
//     overlap returns ErrApplyConflict and consumes nothing; so does any
//     merge failure — apply failure never consumes.
//   - released: mark consumed, keep the sealed state addressable. No
//     workspace change; the scope's lifecycle is the caller's to finish.
//   - discarded: the child scope is discarded (destroying a sandbox it
//     owns); the sealed state is retained as a record.
//
// Select and apply end with a causal merge of the child into the parent,
// which also destroys a child-owned sandbox — safe because the sealed state
// outlives it (git worktrees share the object store; the containerd adapter
// pins captured states on a lease).
//
// Records shepherd.run_output.settled.v1 on the sealing scope's owner path.
// The append is advisory, checkpoint-style: by the time it runs the
// settlement's physical effects have happened and cannot be rolled back.
//
// Select and apply are not transactional across the two systems they touch:
// the workspace effect lands first, and the causal merge (trace append plus
// scope teardown) runs after. If that recording fails, the parent already
// holds the output's changes and the error says so; the output stays
// unconsumed, so the caller can end its lifecycle with SettleRelease or
// SettleDiscard rather than retry a physical effect that already happened.
func (m *ScopeManager) Settle(ctx context.Context, outputID string, action SettlementAction) error {
	m.mu.RLock()
	out, ok := m.outputs[outputID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("retained output %s not found", outputID)
	}

	// Hold the output's lock for the whole validate → act → mark sequence:
	// a concurrent settle of the same output must not run its side effects
	// twice. All failure paths below return before mutating anything.
	out.mu.Lock()
	defer out.mu.Unlock()

	if out.state != OutputUnconsumed {
		return fmt.Errorf("%w: output %s is %s", ErrOutputConsumed, out.id, out.state)
	}
	terminal, err := stateFor(action)
	if err != nil {
		return err
	}

	m.mu.RLock()
	scope, scopeOK := m.scopes[out.scopeID]
	m.mu.RUnlock()
	if !scopeOK {
		return fmt.Errorf("scope %s for output %s not found", out.scopeID, out.id)
	}

	switch action {
	case SettleRelease:
		// No workspace change, no scope lifecycle change: release means
		// "keep, don't integrate". The caller discards or merges the scope
		// separately if it owns resources.

	case SettleDiscard:
		parent := scope.Parent()
		if parent == nil {
			return fmt.Errorf("scope %s for output %s has no parent to discard from", out.scopeID, out.id)
		}
		if err := parent.Discard(scope); err != nil {
			return fmt.Errorf("settle %s (discard): %w", out.id, err)
		}

	case SettleSelect:
		parent, parentSb, err := settleParent(scope, out, "select")
		if err != nil {
			return err
		}
		// Fast-forward-only, strictly: the parent's workspace must be
		// bit-identical to the fork baseline. Digest equality is the honest
		// test — a parent with uncommitted content at the fork point has a
		// capture that can never digest-match (git's stash create mints a
		// fresh commit per capture), so it is told to use apply rather than
		// silently losing that content to ApplyWorkspace's reset.
		parentNow, err := parentSb.Capture(ctx)
		if err != nil {
			return fmt.Errorf("settle %s (select): capture parent: %w", out.id, err)
		}
		baseDigest, err := out.baseline.Digest()
		if err != nil {
			return fmt.Errorf("settle %s (select): digest baseline: %w", out.id, err)
		}
		nowDigest, err := parentNow.Digest()
		if err != nil {
			return fmt.Errorf("settle %s (select): digest parent: %w", out.id, err)
		}
		if nowDigest != baseDigest {
			return fmt.Errorf("settle %s (select): parent workspace has moved since the fork baseline; use SettleApply", out.id)
		}
		if err := parent.ApplyWorkspace(ctx, out.workspace); err != nil {
			return fmt.Errorf("settle %s (select): %w", out.id, err)
		}
		if err := parent.Merge(scope); err != nil {
			// The parent's workspace already holds this output's changes;
			// the causal merge is what failed. Say both, so the caller does
			// not retry a physical effect that already happened.
			return fmt.Errorf("settle %s (select): the parent's workspace now holds this output's changes, but recording the causal merge failed: %w (the output stays unconsumed; end it with SettleRelease or SettleDiscard)", out.id, err)
		}

	case SettleApply:
		parent, parentSb, err := settleParent(scope, out, "apply")
		if err != nil {
			return err
		}
		// The parent's concurrent changes are what it holds now that the
		// baseline does not. Path overlap with the output's own changes
		// means the three-way merge would clobber, so refuse before trying.
		_, parentFiles, err := parentSb.Diff(ctx, out.baseline, 0)
		if err != nil {
			return fmt.Errorf("settle %s (apply): diff parent against baseline: %w", out.id, err)
		}
		if overlap := overlappingPaths(out.changes, parentFiles); len(overlap) > 0 {
			return fmt.Errorf("%w: parent changed %v since the baseline, which the sealed output also changes", ErrApplyConflict, overlap)
		}
		da, canDelta := parentSb.(DeltaApplier)
		if !canDelta {
			return fmt.Errorf("settle %s (apply): parent sandbox backend %s does not implement DeltaApplier: %w",
				out.id, parentSb.Backend(), ErrUnsupported)
		}
		if err := da.ApplyDelta(ctx, out.workspace); err != nil {
			return fmt.Errorf("settle %s (apply): %w", out.id, err)
		}
		if err := parent.Merge(scope); err != nil {
			return fmt.Errorf("settle %s (apply): the parent's workspace now holds this output's changes, but recording the causal merge failed: %w (the output stays unconsumed; end it with SettleRelease or SettleDiscard)", out.id, err)
		}

	default:
		return fmt.Errorf("unknown settlement action %q", action)
	}

	out.state = terminal
	out.action = action
	out.settledAt = time.Now()
	recordRunOutputSettled(ctx, scope, out, action)
	return nil
}

// settleParent resolves and validates the parent side of a select or apply.
// Both verbs physically change the parent workspace and causally merge the
// child, so the child must still be active and the parent must still be an
// active sandboxed scope. No check here mutates anything.
func settleParent(scope *Scope, out *RetainedOutput, verb string) (*Scope, Sandbox, error) {
	if scope.State() != ScopeActive {
		return nil, nil, fmt.Errorf("settle %s (%s): scope %s is %s",
			out.id, verb, scope.ID(), scope.State())
	}
	parent := scope.Parent()
	if parent == nil {
		return nil, nil, fmt.Errorf("settle %s (%s): scope %s has no parent", out.id, verb, scope.ID())
	}
	if parent.State() != ScopeActive {
		return nil, nil, fmt.Errorf("settle %s (%s): parent %s is %s",
			out.id, verb, parent.ID(), parent.State())
	}
	parentSb := parent.Sandbox()
	if parentSb == nil {
		return nil, nil, fmt.Errorf("settle %s (%s): parent %s has no sandbox", out.id, verb, parent.ID())
	}
	return parent, parentSb, nil
}

// overlappingPaths returns the sealed output's changed paths that the parent
// also changed, in the output's change order.
func overlappingPaths(changes []ProposedChange, parentFiles []string) []string {
	if len(changes) == 0 || len(parentFiles) == 0 {
		return nil
	}
	parentSet := make(map[string]bool, len(parentFiles))
	for _, f := range parentFiles {
		parentSet[f] = true
	}
	var overlap []string
	for _, c := range changes {
		if parentSet[c.Path] {
			overlap = append(overlap, c.Path)
		}
	}
	return overlap
}

// recordRunOutputSettled appends the settlement record on the sealing
// scope's owner path, citing the sealed record. Advisory by design: the
// settlement's physical effects have already happened when this runs.
func recordRunOutputSettled(ctx context.Context, scope *Scope, out *RetainedOutput, action SettlementAction) {
	scope.mu.RLock()
	ownerID := scope.ownerID
	scope.mu.RUnlock()

	causedBy := []string{}
	if out.sealedFactID != "" {
		causedBy = []string{out.sealedFactID}
	}
	stateDigest, _ := out.workspace.Digest()

	_, err := scope.store.Append(ctx, TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:settle:%s:%d", ownerID, action, nextCheckpointSeq.Add(1)),
		Groups: []AppendGroup{{
			TraceOwnerID:  ownerID,
			CausalParents: causedBy,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaRunOutputSettled,
				KindLabel: "run_output:settled",
				Payload: map[string]any{
					"output_id":    out.id,
					"action":       string(action),
					"state_digest": stateDigest,
				},
			}},
		}},
	})
	if err != nil {
		slog.Warn("settlement: record settle in trace failed (advisory)", "output", out.id, "action", action, "err", err)
	}
}
