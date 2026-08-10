package shepherd

import (
	"fmt"
	"sync"
)

// ScopeManager tracks active scopes and their lifecycle. It provides
// a centralized registry for creating, looking up, forking, merging,
// and discarding scopes.
//
// The manager is safe for concurrent use.
type ScopeManager struct {
	store  *SQLiteTraceStore
	bus    *EffectBus
	scopes map[string]*Scope
	mu     sync.RWMutex
}

// NewScopeManager creates a scope manager backed by the given store.
func NewScopeManager(store *SQLiteTraceStore, bus *EffectBus) *ScopeManager {
	return &ScopeManager{
		store:  store,
		bus:    bus,
		scopes: make(map[string]*Scope),
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

	scope := NewScope(m.store, m.bus, ownerID)
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
func (m *ScopeManager) Fork(parentID, childOwnerID string, snapshot any) (*Scope, error) {
	m.mu.Lock()
	parent, ok := m.scopes[parentID]
	m.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("parent scope %s not found", parentID)
	}

	child, err := parent.Fork(childOwnerID, snapshot)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.scopes[child.id] = child
	m.mu.Unlock()

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
