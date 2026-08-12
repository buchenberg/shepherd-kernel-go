package shepherd

import (
	"fmt"
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
	checkpoints map[string]*GitCheckpoint
	mu          sync.RWMutex
}

// NewScopeManager creates a scope manager backed by the given store.
// If the store has a bus attached (via WithBus), scope lifecycle events
// will be published automatically.
func NewScopeManager(store *SQLiteTraceStore) *ScopeManager {
	return &ScopeManager{
		store:       store,
		scopes:      make(map[string]*Scope),
		checkpoints: make(map[string]*GitCheckpoint),
	}
}

// Create registers a new root scope for the given trace owner.
// Returns an error if a scope with this ID already exists.
func (m *ScopeManager) Create(ownerID string) (*Scope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("scope:%s", ownerID)
	if _, exists := m.scopes[id]; exists {
		return nil, fmt.Errorf("scope %s already exists", id)
	}

	scope := NewScope(m.store, ownerID)
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
// Returns an error if the parent doesn't exist or a scope with the
// child owner ID already exists.
func (m *ScopeManager) Fork(parentID, childOwnerID string, snapshot any) (*Scope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	parent, ok := m.scopes[parentID]
	if !ok {
		return nil, fmt.Errorf("parent scope %s not found", parentID)
	}

	childID := fmt.Sprintf("scope:%s", childOwnerID)
	if _, exists := m.scopes[childID]; exists {
		return nil, fmt.Errorf("scope %s already exists", childID)
	}

	child, err := parent.Fork(childOwnerID, snapshot)
	if err != nil {
		return nil, err
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

// --- Checkpoint registry ---

// CreateCheckpoint creates a checkpoint for a scope and registers it.
// The checkpoint captures the git workspace state at repoPath plus the
// opaque caller-provided snapshot (typically serialized conversation history).
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

// RestoreCheckpoint restores a registered checkpoint and returns the
// stored conversation snapshot.
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

// LatestCheckpoint returns the most recent valid checkpoint for a scope,
// or nil if none exists.
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
