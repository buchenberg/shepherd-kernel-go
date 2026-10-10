package shepherd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Recovery: rebuild the scope tree, checkpoints, and retained outputs from
// the durable trace after a process restart (plan 05 §4).
//
// The trace is the registry. Scope lifecycle events, checkpoint blobs, and
// settlement records are all durably recorded, so a restarted process loses
// only what was never recorded: conversation snapshots (deliberately opaque,
// deliberately not persisted beyond checkpoint blobs), timestamps on
// recovered outputs, and any root scope that never forked — a root with no
// fork record is invisible to the trace and must be re-created by the host.

// RecoverOptions configures scope recovery.
type RecoverOptions struct {
	// Sandbox re-adoption. Recovery calls it for every recovered root scope
	// and every isolated child, with the backend the trace recorded (empty
	// for roots, which have no record). Return (nil, nil) to recover the
	// scope without a sandbox — deliberate non-adoption, not an error: the
	// host uses it to orphan-clean worktrees it does not want back. An
	// error fails the recovery: half-adopted sandboxes are worse than none.
	//
	// Shared-sandbox children are never resolved: they inherit the parent's
	// re-adopted sandbox, exactly as they inherited it at fork time.
	Resolver func(scopeID, backend string) (Sandbox, error)
}

// resolve is the nil-safe form of opts.Resolver.
func (o RecoverOptions) resolve(scopeID, backend string) (Sandbox, error) {
	if o.Resolver == nil {
		return nil, nil
	}
	return o.Resolver(scopeID, backend)
}

// forkEdge is one recovered fork: parent → child, with what the child needs.
type forkEdge struct {
	parentOwner string
	childOwner  string
	isolated    bool
	backend     string
	baseline    WorkspaceState
	forkPoint   string
}

// RecoverScopes rebuilds a ScopeManager from the durable trace: the scope
// tree and terminal states from lifecycle records, the checkpoint registry
// from the persisted blobs (with staleness detection against the re-adopted
// sandboxes), and retained outputs from sealed/settled records.
//
// What is and is not recoverable:
//
//   - Every scope that appears in a fork record — as parent or child — is
//     recovered, with its lifecycle state (merged/discarded children and
//     halted scopes included: terminal states are data too).
//   - A root scope that never forked left no record and is not recovered;
//     the host re-creates it. RecoverScopes returns the recovered roots so
//     the host can decide whether they are new or forgotten.
//   - Checkpoints recover from the checkpoint_snapshots table. A recovered
//     checkpoint whose recorded workspace revision no longer matches a
//     fresh capture of the re-adopted sandbox is marked CheckpointInvalid —
//     never silently restorable. Revision, not digest: git mints a fresh
//     stash commit per capture, so digest equality would invalidate every
//     dirty-tree checkpoint; revision (the HEAD) is the stable timeline
//     position. Sandboxes that cannot be captured at recovery (no sandbox
//     resolved, capture error) leave their checkpoints Valid — restore
//     re-validates against the live workspace anyway.
//   - Retained outputs recover settle-able: the sealed payload carries the
//     full sealed and baseline states and the change list. A recovered
//     output's Diff is empty (the diff body was never recorded) and its
//     SealedAt/SettledAt are zero times — the records carry no timestamps.
//
// Sandboxes are never invented: the resolver decides what is re-adopted,
// and a recovery without a resolver rebuilds the causal tree only.
func RecoverScopes(store *SQLiteTraceStore, opts RecoverOptions) (*ScopeManager, error) {
	ctx := context.Background()

	edges, err := scanForkRecords(ctx, store)
	if err != nil {
		return nil, err
	}
	terminal, err := scanTerminalStates(ctx, store)
	if err != nil {
		return nil, err
	}

	mgr := NewScopeManager(store)

	// The owner set: everyone who appears in a fork record.
	owners := make(map[string]bool)
	childOwners := make(map[string]bool)
	for _, e := range edges {
		owners[e.parentOwner] = true
		owners[e.childOwner] = true
		childOwners[e.childOwner] = true
	}

	scopes := make(map[string]*Scope, len(owners))
	for owner := range owners {
		state := ScopeActive
		if t, ok := terminal[owner]; ok {
			state = t
		}
		scope := &Scope{
			id:      fmt.Sprintf("scope:%s", owner),
			ownerID: owner,
			store:   store,
			state:   state,
		}
		scopes[owner] = scope
	}

	// Link the tree before anything else needs the parent pointers.
	for _, e := range edges {
		parent := scopes[e.parentOwner]
		child := scopes[e.childOwner]
		child.parent = parent
		child.forkPoint = e.forkPoint
		child.baseline = e.baseline
		parent.mu.Lock()
		parent.children = append(parent.children, child)
		parent.mu.Unlock()
	}

	// Register the tree, then load checkpoints and outputs — either can
	// introduce a scope the fork scan cannot see: a root that never forked
	// left no lifecycle record, but its checkpoint and sealed-output rows
	// prove it existed. Such scopes recover as bare active roots.
	mgr.mu.Lock()
	for _, scope := range scopes {
		mgr.scopes[scope.id] = scope
	}
	mgr.mu.Unlock()

	if err := recoverCheckpoints(ctx, mgr, store); err != nil {
		return nil, err
	}
	if err := recoverOutputs(ctx, mgr, store); err != nil {
		return nil, err
	}

	// Attach sandboxes: roots via the resolver (not owned, matching
	// Create), isolated children via the resolver (owned, matching
	// ForkIsolated), shared children inherit the parent's — including
	// bare roots introduced by checkpoint/output records.
	mgr.mu.RLock()
	all := make([]*Scope, 0, len(mgr.scopes))
	for _, scope := range mgr.scopes {
		all = append(all, scope)
	}
	mgr.mu.RUnlock()

	// Pass 1 resolves roots; pass 2 resolves isolated children and lets
	// shared children inherit — inheritance must see the parent's
	// re-adopted sandbox, so the order between the passes is not optional.
	roots := 0
	for _, scope := range all {
		if isChild := func() bool {
			scope.mu.Lock()
			defer scope.mu.Unlock()
			return scope.parent != nil
		}(); isChild {
			continue
		}
		sb, err := opts.resolve(scope.id, "")
		if err != nil {
			return nil, fmt.Errorf("recover scope %s: re-adopt sandbox: %w", scope.id, err)
		}
		if sb != nil {
			scope.WithSandbox(sb, false)
		}
		roots++
	}
	for _, scope := range all {
		if isChild := func() bool {
			scope.mu.Lock()
			defer scope.mu.Unlock()
			return scope.parent != nil
		}(); !isChild {
			continue
		}
		if edge := edgeFor(edges, scope.ownerID); edge != nil && edge.isolated {
			sb, err := opts.resolve(scope.id, edge.backend)
			if err != nil {
				return nil, fmt.Errorf("recover scope %s: re-adopt sandbox: %w", scope.id, err)
			}
			if sb != nil {
				scope.WithSandbox(sb, true)
			}
			continue
		}
		// A shared child inherits its parent's re-adopted sandbox, exactly
		// as it inherited the parent's at fork time; there is nothing to
		// resolve for it.
		if parent := scope.Parent(); parent != nil {
			scope.mu.Lock()
			scope.sandbox = parent.Sandbox()
			scope.mu.Unlock()
		}
	}

	// Staleness runs after re-adoption: it needs the sandboxes.
	if err := markStaleCheckpoints(ctx, mgr); err != nil {
		return nil, err
	}

	slog.Info("recovery: rebuilt scope registry from trace",
		"scopes", len(all), "roots", roots)
	return mgr, nil
}

// ensureScope returns the manager's scope for ownerID, creating and
// registering a bare active root when it does not exist — the shape a
// never-forked root recovers as.
func ensureScope(mgr *ScopeManager, store *SQLiteTraceStore, ownerID string) *Scope {
	scopeID := fmt.Sprintf("scope:%s", ownerID)
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if scope, ok := mgr.scopes[scopeID]; ok {
		return scope
	}
	scope := &Scope{
		id:      scopeID,
		ownerID: ownerID,
		store:   store,
		state:   ScopeActive,
	}
	mgr.scopes[scopeID] = scope
	return scope
}

// scanForkRecords reads every fork record into edges. The parent is the
// record's owner path; the child, isolation, backend, and full baseline
// state come from the payload (baseline entries older than the recovery
// payload keys decode empty, and those children simply cannot propose
// merges — the same refusal a pure-causal fork gets).
func scanForkRecords(ctx context.Context, store *SQLiteTraceStore) ([]forkEdge, error) {
	records, err := store.scanRecordsBySchema(ctx, SchemaScopeForked)
	if err != nil {
		return nil, fmt.Errorf("scan fork records: %w", err)
	}

	var edges []forkEdge
	for _, rec := range records {
		childOwner, _ := rec.Payload["child_owner"].(string)
		if childOwner == "" {
			return nil, fmt.Errorf("fork record %s on %s has no child_owner", rec.RecordID, rec.OwnerID)
		}
		edge := forkEdge{
			parentOwner: rec.OwnerID,
			childOwner:  childOwner,
			forkPoint:   rec.RecordID,
			isolated:    rec.Payload["isolated"] == true,
		}
		edge.backend, _ = rec.Payload["sandbox_backend"].(string)
		if baselineRaw, ok := rec.Payload["baseline"].(map[string]any); ok {
			edge.baseline = workspaceStateFromPayload(baselineRaw)
		}
		edges = append(edges, edge)
	}
	return edges, nil
}

// scanTerminalStates folds merged/discarded/halt records into a per-owner
// terminal state. A child merged then somehow discarded cannot happen (the
// state machine rejects it), so a collision is a corrupt trace and fails
// the recovery loudly.
func scanTerminalStates(ctx context.Context, store *SQLiteTraceStore) (map[string]ScopeState, error) {
	records, err := store.scanRecordsBySchema(ctx, SchemaScopeMerged, SchemaScopeDiscarded, SchemaSupervisorHalt)
	if err != nil {
		return nil, fmt.Errorf("scan terminal records: %w", err)
	}

	states := make(map[string]ScopeState)
	for _, rec := range records {
		var owner string
		var state ScopeState
		switch rec.SchemaRef {
		case SchemaSupervisorHalt:
			// Halt is recorded on the halted scope's own path.
			owner = rec.OwnerID
			state = ScopeDiscarded
		case SchemaScopeMerged, SchemaScopeDiscarded:
			// Merges and discards are recorded on the parent's path; the
			// subject is the child_owner in the payload.
			child, _ := rec.Payload["child_owner"].(string)
			owner = child
			if rec.SchemaRef == SchemaScopeMerged {
				state = ScopeMerged
			} else {
				state = ScopeDiscarded
			}
		}
		if owner == "" {
			continue
		}
		if prior, ok := states[owner]; ok && prior != state {
			return nil, fmt.Errorf("owner %s is recorded both %s and %s: the trace is inconsistent", owner, prior, state)
		}
		states[owner] = state
	}
	return states, nil
}

// edgeFor returns the edge that made owner a child, or nil.
func edgeFor(edges []forkEdge, childOwner string) *forkEdge {
	for i := range edges {
		if edges[i].childOwner == childOwner {
			return &edges[i]
		}
	}
	return nil
}

// recoverCheckpoints rebuilds the checkpoint registry from the persisted
// blobs. A checkpoint whose scope is unknown to the fork scan introduces a
// bare root scope: the blob proves the scope existed even though it never
// left a lifecycle record.
func recoverCheckpoints(ctx context.Context, mgr *ScopeManager, store *SQLiteTraceStore) error {
	blobs, corrupt, err := store.loadCheckpointBlobs(ctx)
	if err != nil {
		return fmt.Errorf("recover checkpoints: %w", err)
	}
	for _, id := range corrupt {
		slog.Warn("recovery: skipping checkpoint with undecodable state row", "checkpoint", id)
	}

	for _, b := range blobs {
		ensureScope(mgr, store, strings.TrimPrefix(b.ScopeID, "scope:"))

		cp := &Checkpoint{
			ID:        b.CheckpointID,
			Seq:       b.Seq,
			ScopeID:   b.ScopeID,
			Workspace: b.State,
			Snapshot:  b.Snapshot,
			CreatedAt: b.CreatedAt,
			State:     CheckpointValid,
		}

		mgr.mu.Lock()
		if existing, exists := mgr.checkpoints[cp.ID]; exists {
			mgr.mu.Unlock()
			return fmt.Errorf("recovered checkpoint ID collision: %s (scopes %s, %s)", cp.ID, existing.ScopeID, cp.ScopeID)
		}
		mgr.checkpoints[cp.ID] = cp
		mgr.mu.Unlock()
	}
	return nil
}

// markStaleCheckpoints invalidates recovered checkpoints whose recorded
// revision no longer matches a fresh capture of the re-adopted sandbox —
// the never-silently-restorable rule (plan 05 section 4). Revision, not
// digest: git mints a fresh stash commit per capture, so digest equality
// would invalidate every dirty-tree checkpoint; the revision is the
// stable timeline position. A scope without a re-adopted sandbox leaves
// its checkpoints Valid — restore re-validates against the live
// workspace anyway.
func markStaleCheckpoints(ctx context.Context, mgr *ScopeManager) error {
	mgr.mu.RLock()
	type pair struct {
		cp    *Checkpoint
		scope *Scope
	}
	var pairs []pair
	for _, cp := range mgr.checkpoints {
		if scope, ok := mgr.scopes[cp.ScopeID]; ok {
			pairs = append(pairs, pair{cp, scope})
		}
	}
	mgr.mu.RUnlock()

	for _, p := range pairs {
		p.scope.mu.RLock()
		sb := p.scope.sandbox
		p.scope.mu.RUnlock()
		if sb == nil {
			continue
		}
		now, err := sb.Capture(ctx)
		if err != nil {
			slog.Warn("recovery: could not capture workspace for checkpoint staleness check",
				"checkpoint", p.cp.ID, "err", err)
			continue
		}
		p.cp.mu.Lock()
		if p.cp.State == CheckpointValid && now.Revision != p.cp.Workspace.Revision {
			p.cp.State = CheckpointInvalid
		}
		p.cp.mu.Unlock()
	}
	return nil
}

// recoverOutputs rebuilds the retained-output registry from sealed and
// settled records. Sealed payloads carry the full states and change list,
// so a recovered output is settle-able; settled records mark the terminal
// ones.
func recoverOutputs(ctx context.Context, mgr *ScopeManager, store *SQLiteTraceStore) error {
	sealed, err := store.scanRecordsBySchema(ctx, SchemaRunOutputSealed)
	if err != nil {
		return fmt.Errorf("recover outputs: scan sealed records: %w", err)
	}
	settled, err := store.scanRecordsBySchema(ctx, SchemaRunOutputSettled)
	if err != nil {
		return fmt.Errorf("recover outputs: scan settled records: %w", err)
	}

	terminals := make(map[string]string) // output id -> action
	for _, rec := range settled {
		id, _ := rec.Payload["output_id"].(string)
		action, _ := rec.Payload["action"].(string)
		if id != "" {
			terminals[id] = action
		}
	}

	for _, rec := range sealed {
		id, _ := rec.Payload["output_id"].(string)
		if id == "" {
			continue
		}
		scopeID, _ := rec.Payload["scope"].(string)
		if owner := strings.TrimPrefix(scopeID, "scope:"); owner != "" {
			ensureScope(mgr, store, owner)
		}

		out := &RetainedOutput{
			id:      id,
			scopeID: scopeID,
			state:   OutputUnconsumed,
		}
		if stateRaw, ok := rec.Payload["state"].(map[string]any); ok {
			out.workspace = workspaceStateFromPayload(stateRaw)
		}
		if baseRaw, ok := rec.Payload["baseline"].(map[string]any); ok {
			out.baseline = workspaceStateFromPayload(baseRaw)
		}
		if changesRaw, ok := rec.Payload["changes"].([]any); ok {
			for _, cRaw := range changesRaw {
				c, ok := cRaw.(map[string]any)
				if !ok {
					continue
				}
				path, _ := c["path"].(string)
				kind, _ := c["kind"].(string)
				out.changes = append(out.changes, ProposedChange{Path: path, Kind: kind})
			}
		}
		out.sealedFactID = rec.RecordID

		if action, ok := terminals[id]; ok {
			terminal, err := stateFor(SettlementAction(action))
			if err != nil {
				slog.Warn("recovery: unknown settlement action on record; output left unconsumed",
					"output", id, "action", action)
			} else {
				out.state = terminal
				out.action = SettlementAction(action)
			}
		}

		mgr.mu.Lock()
		if existing, exists := mgr.outputs[out.id]; exists {
			mgr.mu.Unlock()
			return fmt.Errorf("recovered output ID collision: %s (scopes %s, %s)", out.id, existing.scopeID, out.scopeID)
		}
		out.mgr = mgr
		mgr.outputs[out.id] = out
		mgr.mu.Unlock()
	}
	return nil
}

// workspaceStateFromPayload decodes a state encoded by the lifecycle
// payloads ({"backend", "revision", "data"}).
func workspaceStateFromPayload(raw map[string]any) WorkspaceState {
	ws := WorkspaceState{}
	ws.Backend, _ = raw["backend"].(string)
	ws.Revision, _ = raw["revision"].(string)
	if data, ok := raw["data"].(map[string]any); ok {
		ws.Data = data
	}
	if ws.Backend == "" {
		return WorkspaceState{}
	}
	return ws
}
