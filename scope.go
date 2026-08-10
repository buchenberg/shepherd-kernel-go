package shepherd

import (
	"fmt"
	"sync"
	"time"
)

// Scope schema refs for trace recording.
const (
	SchemaScopeForked    = "shepherd.scope.forked.v1"
	SchemaScopeMerged    = "shepherd.scope.merged.v1"
	SchemaScopeDiscarded = "shepherd.scope.discarded.v1"
)

// ScopeState tracks the lifecycle of a scope.
type ScopeState string

const (
	ScopeActive    ScopeState = "active"
	ScopeMerged    ScopeState = "merged"
	ScopeDiscarded ScopeState = "discarded"
)

// Scope represents a branch of agent execution. It wraps a trace owner
// and supports fork/merge/discard over the causal graph.
//
// Unlike the Python Shepherd, this scope does NOT carry filesystem state
// or context bindings — yaah sub-agents are independent LLM loops, not
// shared-fs containers. "Fork" means "branch the causal graph and start
// a new execution from this point."
//
// The scope records lifecycle events (forked, merged, discarded) in the
// trace store so the supervisor can inspect the full branching history.
type Scope struct {
	id        string
	ownerID   string
	store     *SQLiteTraceStore
	bus       *EffectBus
	parent    *Scope
	forkPoint string   // record ID at fork time (empty for root scopes)
	children  []*Scope
	state     ScopeState
	mu        sync.RWMutex
}

// NewScope creates a root scope (no parent) for the given trace owner.
func NewScope(store *SQLiteTraceStore, bus *EffectBus, ownerID string) *Scope {
	return &Scope{
		id:      fmt.Sprintf("scope:%s", ownerID),
		ownerID: ownerID,
		store:   store,
		bus:     bus,
		state:   ScopeActive,
	}
}

// ID returns the scope's unique identifier.
func (s *Scope) ID() string { return s.id }

// OwnerID returns the trace owner ID this scope writes to.
func (s *Scope) OwnerID() string { return s.ownerID }

// State returns the current lifecycle state.
func (s *Scope) State() ScopeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Parent returns the parent scope, or nil for root scopes.
func (s *Scope) Parent() *Scope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.parent
}

// ForkPoint returns the record ID at which this scope was forked,
// or empty string for root scopes.
func (s *Scope) ForkPoint() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.forkPoint
}

// Children returns a copy of the child scopes.
func (s *Scope) Children() []*Scope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]*Scope, len(s.children))
	copy(cp, s.children)
	return cp
}

// Fork creates a child scope branched from the current scope's state.
// The child gets a new trace owner ID and inherits the parent's causal
// context — its first record cites the parent's head as a causal parent.
//
// A "scope.forked" declaration is recorded in the parent's trace.
func (s *Scope) Fork(childOwnerID string) (*Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != ScopeActive {
		return nil, fmt.Errorf("cannot fork scope %s in state %s", s.id, s.state)
	}

	// Record the fork event in the parent's trace to get a fork-point record.
	forkReceipt, err := s.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:fork:%s:%d", s.ownerID, childOwnerID, time.Now().UnixNano()),
		Groups: []AppendGroup{{
			TraceOwnerID: s.ownerID,
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: SchemaScopeForked,
				KindLabel: "scope:forked",
				Payload: map[string]any{
					"child_owner": childOwnerID,
				},
			}},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("record fork event: %w", err)
	}

	forkPointID := ""
	if len(forkReceipt.FactIDs) > 0 {
		forkPointID = forkReceipt.FactIDs[0]
	}

	child := &Scope{
		id:        fmt.Sprintf("scope:%s", childOwnerID),
		ownerID:   childOwnerID,
		store:     s.store,
		bus:       s.bus,
		parent:    s,
		forkPoint: forkPointID,
		state:     ScopeActive,
	}

	s.children = append(s.children, child)
	return child, nil
}

// Merge propagates the child's effects into this (parent) scope.
// Records a "scope.merged" capture in the parent's trace, citing the
// child's fork point as a causal parent. Marks the child as merged.
func (s *Scope) Merge(child *Scope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	child.mu.Lock()
	defer child.mu.Unlock()

	if s.state != ScopeActive {
		return fmt.Errorf("cannot merge into scope %s in state %s", s.id, s.state)
	}
	if child.state != ScopeActive {
		return fmt.Errorf("cannot merge scope %s in state %s", child.id, child.state)
	}
	if child.parent != s {
		return fmt.Errorf("scope %s is not a child of %s", child.id, s.id)
	}

	// Record merge event in parent's trace, citing the child's fork point.
	causedBy := []string{}
	if child.forkPoint != "" {
		causedBy = []string{child.forkPoint}
	}

	_, err := s.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:merge:%s:%d", s.ownerID, child.ownerID, time.Now().UnixNano()),
		Groups: []AppendGroup{{
			TraceOwnerID:  s.ownerID,
			CausalParents: causedBy,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaScopeMerged,
				KindLabel: "scope:merged",
				Payload: map[string]any{
					"child_owner": child.ownerID,
					"child_scope": child.id,
				},
			}},
		}},
	})
	if err != nil {
		return fmt.Errorf("record merge event: %w", err)
	}

	child.state = ScopeMerged
	return nil
}

// Discard abandons the child scope's effects. Records a "scope.discarded"
// capture in the parent's trace. The child's records remain in the store
// (append-only) but are semantically abandoned.
func (s *Scope) Discard(child *Scope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	child.mu.Lock()
	defer child.mu.Unlock()

	if s.state != ScopeActive {
		return fmt.Errorf("cannot discard from scope %s in state %s", s.id, s.state)
	}
	if child.state != ScopeActive {
		return fmt.Errorf("cannot discard scope %s in state %s", child.id, child.state)
	}
	if child.parent != s {
		return fmt.Errorf("scope %s is not a child of %s", child.id, s.id)
	}

	causedBy := []string{}
	if child.forkPoint != "" {
		causedBy = []string{child.forkPoint}
	}

	_, err := s.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:discard:%s:%d", s.ownerID, child.ownerID, time.Now().UnixNano()),
		Groups: []AppendGroup{{
			TraceOwnerID:  s.ownerID,
			CausalParents: causedBy,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaScopeDiscarded,
				KindLabel: "scope:discarded",
				Payload: map[string]any{
					"child_owner": child.ownerID,
					"child_scope": child.id,
				},
			}},
		}},
	})
	if err != nil {
		return fmt.Errorf("record discard event: %w", err)
	}

	child.state = ScopeDiscarded
	return nil
}
