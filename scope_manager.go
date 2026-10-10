package shepherd

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// ScopeManager tracks active scopes and their lifecycle. It provides
// a centralized registry for creating, looking up, forking, merging,
// discarding scopes, and managing checkpoints.
//
// The manager is safe for concurrent use.
type ScopeManager struct {
	store       *SQLiteTraceStore
	scopes      map[string]*Scope
	checkpoints map[string]*Checkpoint
	outputs     map[string]*RetainedOutput
	mu          sync.RWMutex
}

// NewScopeManager creates a scope manager backed by the given store.
// If the store has a bus attached (via WithBus), scope lifecycle events
// will be published automatically.
func NewScopeManager(store *SQLiteTraceStore) *ScopeManager {
	return &ScopeManager{
		store:       store,
		scopes:      make(map[string]*Scope),
		checkpoints: make(map[string]*Checkpoint),
		outputs:     make(map[string]*RetainedOutput),
	}
}

// Create registers a new root scope for the given trace owner and attaches sb
// as its execution substrate. Pass a nil sandbox for a pure-causal scope, whose
// workspace and checkpoint operations will return ErrNoSandbox.
//
// Returns an error if a scope with this ID already exists.
func (m *ScopeManager) Create(ownerID string, sb Sandbox) (*Scope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("scope:%s", ownerID)
	if _, exists := m.scopes[id]; exists {
		return nil, fmt.Errorf("scope %s already exists", id)
	}

	scope := NewScope(m.store, ownerID).WithSandbox(sb, false)
	m.scopes[id] = scope
	return scope, nil
}

// Get returns a scope by its ID, or false if not found.
func (m *ScopeManager) Get(id string) (*Scope, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.scopes[id]
	return s, ok
}

// Fork creates a child scope branched from the parent. The child is
// automatically registered with the manager. The snapshot parameter
// captures execution state at fork time (pass nil if not needed).
//
// The fork baseline (the parent's workspace state) is captured BEFORE the
// manager's write lock is taken: a capture is a backend subprocess bounded
// only by its own timeout (git: up to 30s), and the registry must not block
// every other operation on it. The parent is re-validated inside
// forkWithBaseline, so a parent discarded during the capture still fails
// the fork.
//
// Returns an error if the parent doesn't exist or a scope with the
// child owner ID already exists.
func (m *ScopeManager) Fork(parentID, childOwnerID string, snapshot any) (*Scope, error) {
	return m.forkScope(parentID, childOwnerID, snapshot, false, nil)
}

// ForkIsolated creates a child scope with its own execution substrate instead of
// inheriting the parent's. The child owns sb, so Discard, Merge, and Halt
// destroy it — this is what makes a discard a physical rollback for an
// isolating backend such as a git worktree. The isolation is recorded in the
// fork payload, which is what recovery uses to decide that a re-adopted
// sandbox belongs to this child alone.
//
// Returns an error if the parent doesn't exist or a scope with the child owner
// ID already exists.
func (m *ScopeManager) ForkIsolated(parentID, childOwnerID string, snapshot any, sb Sandbox) (*Scope, error) {
	return m.forkScope(parentID, childOwnerID, snapshot, true, sb)
}

// forkScope is the shared fork path: resolve the parent, capture the fork
// baseline outside the registry lock, then register atomically. isolated
// forks record their own substrate (sb may be nil for shared forks).
func (m *ScopeManager) forkScope(parentID, childOwnerID string, snapshot any, isolated bool, sb Sandbox) (*Scope, error) {
	m.mu.RLock()
	parent, ok := m.scopes[parentID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("parent scope %s not found", parentID)
	}

	baseline, err := parent.forkBaseline()
	if err != nil {
		return nil, err
	}

	backend := ""
	if isolated && sb != nil {
		backend = sb.Backend()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	childID := fmt.Sprintf("scope:%s", childOwnerID)
	if _, exists := m.scopes[childID]; exists {
		return nil, fmt.Errorf("scope %s already exists", childID)
	}

	child, err := parent.forkWithBaseline(childOwnerID, snapshot, baseline, isolated, backend)
	if err != nil {
		return nil, err
	}
	if isolated && sb != nil {
		child.WithSandbox(sb, true)
	}

	m.scopes[child.id] = child
	return child, nil
}

// Merge propagates a child scope's effects into its parent.
func (m *ScopeManager) Merge(childID string) error {
	m.mu.RLock()
	child, ok := m.scopes[childID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("scope %s not found", childID)
	}

	parent := child.Parent()
	if parent == nil {
		return fmt.Errorf("scope %s has no parent (root scope)", childID)
	}

	return parent.Merge(child)
}

// Discard abandons a child scope's effects.
func (m *ScopeManager) Discard(childID string) error {
	m.mu.RLock()
	child, ok := m.scopes[childID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("scope %s not found", childID)
	}

	parent := child.Parent()
	if parent == nil {
		return fmt.Errorf("scope %s has no parent (root scope)", childID)
	}

	return parent.Discard(child)
}

// ActiveScopes returns all scopes in the Active state.
func (m *ScopeManager) ActiveScopes() []*Scope {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var active []*Scope
	for _, s := range m.scopes {
		if s.State() == ScopeActive {
			active = append(active, s)
		}
	}
	return active
}

// AllScopes returns all registered scopes regardless of state.
func (m *ScopeManager) AllScopes() []*Scope {
	m.mu.RLock()
	defer m.mu.RUnlock()

	all := make([]*Scope, 0, len(m.scopes))
	for _, s := range m.scopes {
		all = append(all, s)
	}
	return all
}

// --- Retained output registry ---

// Seal seals a scope's workspace as a retained output and registers it, which
// is what makes the output settle-able: Settle and RetainedOutput.Settle
// resolve the output and its scopes through the manager. The scope stays
// active — sealing does not merge.
func (m *ScopeManager) Seal(ctx context.Context, scopeID string) (*RetainedOutput, error) {
	m.mu.RLock()
	scope, ok := m.scopes[scopeID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("scope %s not found", scopeID)
	}

	out, err := scope.Seal(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, exists := m.outputs[out.id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("output ID collision: %s already exists (scope %s)", out.id, existing.scopeID)
	}
	out.mgr = m
	m.outputs[out.id] = out
	m.mu.Unlock()

	return out, nil
}

// Output returns a registered retained output by ID, or false if not found.
func (m *ScopeManager) Output(id string) (*RetainedOutput, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out, ok := m.outputs[id]
	return out, ok
}

// OutputsForScope returns the retained outputs sealed by a scope, oldest
// first (by seal sequence).
func (m *ScopeManager) OutputsForScope(scopeID string) []*RetainedOutput {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var outs []*RetainedOutput
	for _, out := range m.outputs {
		if out.scopeID == scopeID {
			outs = append(outs, out)
		}
	}
	sort.Slice(outs, func(i, j int) bool {
		return outs[i].sealedAt.Before(outs[j].sealedAt)
	})
	return outs
}

// --- Checkpoint registry ---

// CreateCheckpoint creates a checkpoint for a scope and registers it. The
// checkpoint captures the scope's workspace state via its sandbox, plus the
// opaque caller-provided snapshot (typically serialized conversation history).
//
// The manager lock is NOT held during the workspace capture — resolving the
// scope takes a read lock, the (potentially slow) capture runs outside it, and
// only the final map registration takes the write lock.
func (m *ScopeManager) CreateCheckpoint(ctx context.Context, scopeID string, snapshot []byte) (*Checkpoint, error) {
	m.mu.RLock()
	scope, ok := m.scopes[scopeID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("scope %s not found", scopeID)
	}

	cp, err := scope.CreateCheckpoint(ctx, snapshot)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, exists := m.checkpoints[cp.ID]; exists {
		// Should be impossible with a monotonic sequence, but reject an
		// unexpected ID collision rather than silently replacing a checkpoint.
		m.mu.Unlock()
		return nil, fmt.Errorf("checkpoint ID collision: %s already exists (scope %s)", cp.ID, existing.ScopeID)
	}
	m.checkpoints[cp.ID] = cp
	m.mu.Unlock()

	return cp, nil
}

// RestoreCheckpoint restores a registered checkpoint and returns the
// stored conversation snapshot.
func (m *ScopeManager) RestoreCheckpoint(ctx context.Context, checkpointID string) ([]byte, error) {
	m.mu.RLock()
	cp, ok := m.checkpoints[checkpointID]
	var scope *Scope
	if ok {
		scope, ok = m.scopes[cp.ScopeID]
	}
	m.mu.RUnlock()

	if !ok {
		if cp == nil {
			return nil, fmt.Errorf("checkpoint %s not found", checkpointID)
		}
		return nil, fmt.Errorf("scope %s for checkpoint %s not found", cp.ScopeID, checkpointID)
	}

	return scope.RestoreCheckpoint(ctx, cp)
}

// DestroyScopeSandbox tears down a scope's owned sandbox without changing the
// scope's lifecycle state. Callers use this on the abort path to release
// worktrees and other provisioned resources when a session ends without a
// merge or discard.
//
// It is a no-op for scopes with no sandbox or one they do not own, so an
// in-place materializer over the user's repository is never touched.
func (m *ScopeManager) DestroyScopeSandbox(ctx context.Context, scopeID string) error {
	m.mu.RLock()
	scope, ok := m.scopes[scopeID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("scope %s not found", scopeID)
	}

	scope.mu.RLock()
	sb := scope.sandbox
	owns := scope.ownsSandbox
	scope.mu.RUnlock()

	if sb == nil || !owns {
		return nil
	}
	if err := sb.Destroy(ctx); err != nil {
		return fmt.Errorf("destroy sandbox for scope %s: %w", scopeID, err)
	}
	return nil
}

// LatestCheckpoint returns the most recent valid checkpoint for a scope,
// or nil if none exists. Only checkpoints still in the Valid state are
// considered — a used or invalid checkpoint is not a candidate for restore.
//
// Ordering is by the monotonic Seq (creation order), not CreatedAt, which
// can tie for concurrent checkpoints and would leave selection ambiguous.
func (m *ScopeManager) LatestCheckpoint(scopeID string) *Checkpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var latest *Checkpoint
	for _, cp := range m.checkpoints {
		if cp.ScopeID == scopeID && cp.State == CheckpointValid {
			if latest == nil || cp.Seq > latest.Seq {
				latest = cp
			}
		}
	}
	return latest
}

// PruneCheckpoints removes all checkpoints belonging to a scope and deletes
// their persisted blobs. Call this when a scope is discarded or merged so
// consumed checkpoint metadata (and any still-retained snapshot) is not held
// for the process lifetime — or in the database — after the scope is gone.
// RestoreCheckpoint already releases the snapshot on use; this handles
// checkpoints that were never restored.
func (m *ScopeManager) PruneCheckpoints(ctx context.Context, scopeID string) {
	m.mu.Lock()
	var pruned []*Checkpoint
	for id, cp := range m.checkpoints {
		if cp.ScopeID == scopeID {
			pruned = append(pruned, cp)
			cp.mu.Lock()
			cp.Snapshot = nil
			cp.mu.Unlock()
			delete(m.checkpoints, id)
		}
	}
	m.mu.Unlock()

	// Blob deletion runs outside the registry lock (database I/O) and is
	// advisory: a failed delete must not resurrect an in-memory prune.
	for _, cp := range pruned {
		if err := m.store.deleteCheckpointBlob(ctx, cp.ID); err != nil {
			slog.Warn("prune checkpoints: delete persisted blob failed (advisory)", "checkpoint", cp.ID, "err", err)
		}
	}
}

// PruneTerminal removes merged and discarded scopes from the registry
// (plan 05 §4). Terminal scopes stay lookupable until pruned — their state
// is settled data a supervisor may still read — but a long-lived host that
// accumulates them grows the map without bound.
//
// Pruning a scope does not touch its checkpoints or retained outputs:
// those have their own registries and lifecycles (PruneCheckpoints,
// Settle). The trace records survive either way. Returns the number of
// scopes pruned.
func (m *ScopeManager) PruneTerminal() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	pruned := 0
	for id, scope := range m.scopes {
		if state := scope.State(); state == ScopeMerged || state == ScopeDiscarded {
			delete(m.scopes, id)
			pruned++
		}
	}
	return pruned
}
