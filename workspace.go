package shepherd

import (
	"context"
	"fmt"
	"log/slog"
)

// Workspace schema refs for trace recording. These replace the git-specific
// shepherd.tree.* schemas: the payload is backend-neutral and carries a state
// digest rather than stash/head SHAs.
const (
	SchemaWorkspaceCaptured = "shepherd.workspace.captured.v1"
	SchemaWorkspaceApplied  = "shepherd.workspace.applied.v1"
)

// CaptureWorkspace records the scope's current workspace state as a reusable
// WorkspaceState.
//
// Unlike CreateCheckpoint, this has no lifecycle: it consumes nothing, records
// no checkpoint, and the returned state stays valid indefinitely. This is what
// fork-and-choose needs — snapshot the fork point, run variant A, re-apply the
// fork point, run variant B, then apply the winner.
//
// Records a "workspace.captured" declaration in the scope's trace (advisory).
func (s *Scope) CaptureWorkspace(ctx context.Context) (WorkspaceState, error) {
	s.mu.RLock()
	state := s.state
	sb := s.sandbox
	ownerID := s.ownerID
	scopeID := s.id
	s.mu.RUnlock()

	if state != ScopeActive {
		return WorkspaceState{}, fmt.Errorf("cannot capture workspace in scope %s state %s", scopeID, state)
	}
	if sb == nil {
		return WorkspaceState{}, ErrNoSandbox
	}

	ws, err := sb.Capture(ctx)
	if err != nil {
		return WorkspaceState{}, fmt.Errorf("capture workspace: %w", err)
	}

	s.recordWorkspaceEvent(ctx, ownerID, Declaration, SchemaWorkspaceCaptured, "workspace:captured", ws)
	return ws, nil
}

// ApplyWorkspace resets the scope's workspace to a previously captured state.
//
// The state is not consumed — the same WorkspaceState can be applied repeatedly
// by backends whose workspace state is reusable (the git backend). A backend
// that provisions fresh resources on Apply may make it single-use; see
// Sandbox.Apply.
//
// Records a "workspace.applied" capture in the scope's trace (advisory).
func (s *Scope) ApplyWorkspace(ctx context.Context, ws WorkspaceState) error {
	s.mu.RLock()
	state := s.state
	sb := s.sandbox
	ownerID := s.ownerID
	scopeID := s.id
	s.mu.RUnlock()

	if state != ScopeActive {
		return fmt.Errorf("cannot apply workspace in scope %s state %s", scopeID, state)
	}
	if sb == nil {
		return ErrNoSandbox
	}

	if err := sb.Apply(ctx, ws); err != nil {
		return fmt.Errorf("apply workspace: %w", err)
	}

	s.recordWorkspaceEvent(ctx, ownerID, Capture, SchemaWorkspaceApplied, "workspace:applied", ws)
	return nil
}

// DiffWorkspace returns the unified diff and changed file paths between ws and
// the scope's current workspace. maxLines <= 0 means unbounded.
//
// This is a query: it records no trace event and consumes nothing.
func (s *Scope) DiffWorkspace(ctx context.Context, ws WorkspaceState, maxLines int) (string, []string, error) {
	s.mu.RLock()
	state := s.state
	sb := s.sandbox
	scopeID := s.id
	s.mu.RUnlock()

	if state != ScopeActive {
		return "", nil, fmt.Errorf("cannot diff workspace in scope %s state %s", scopeID, state)
	}
	if sb == nil {
		return "", nil, ErrNoSandbox
	}

	return sb.Diff(ctx, ws, maxLines)
}

// recordWorkspaceEvent appends a workspace lifecycle record. Failure is
// advisory: the workspace operation has already succeeded, so a trace problem
// must not fail the caller.
func (s *Scope) recordWorkspaceEvent(
	ctx context.Context,
	ownerID string,
	mode RecordMode,
	schemaRef, kindLabel string,
	ws WorkspaceState,
) {
	seq := nextCheckpointSeq.Add(1)
	digest, _ := ws.Digest()

	_, err := s.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:workspace:%s:%d", ownerID, kindLabel, seq),
		Groups: []AppendGroup{{
			TraceOwnerID: ownerID,
			FactDrafts: []RecordDraft{{
				Mode:      mode,
				SchemaRef: schemaRef,
				KindLabel: kindLabel,
				Payload: map[string]any{
					"backend":      ws.Backend,
					"revision":     ws.Revision,
					"state_digest": digest,
				},
			}},
		}},
	})
	if err != nil {
		slog.Warn("workspace: record in trace failed (advisory)", "kind", kindLabel, "err", err)
	}
}
