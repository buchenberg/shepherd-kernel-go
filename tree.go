package shepherd

import (
	"fmt"
	"log/slog"
	"strings"
)

// Tree schema refs for trace recording.
const (
	SchemaTreeCaptured = "shepherd.tree.captured.v1"
	SchemaTreeApplied  = "shepherd.tree.applied.v1"
)

// TreeState captures a git workspace state without checkpoint lifecycle.
//
// It is the non-consuming counterpart to GitCheckpoint: capture records
// the stash/HEAD pair, apply restores it, and neither marks anything
// used — the same TreeState can be applied any number of times. This is
// what fork-and-choose needs: snapshot the fork point, run variant A,
// re-apply the fork point, run variant B, then apply the winner.
//
// The StashSHA comes from `git stash create`, which produces an
// unreferenced commit (no refs/stash entry). It survives until git's
// default gc grace period elapses (~2 weeks), which far exceeds any
// interactive session lifetime.
type TreeState struct {
	// HeadSHA is the git HEAD at capture time.
	HeadSHA string
	// StashSHA is the stash commit SHA capturing uncommitted changes.
	// Empty when the tree was clean at capture time.
	StashSHA string
}

// CaptureTree records the current workspace state of repoPath as a
// reusable TreeState. The workspace is left unchanged (the stash is
// created, not popped).
//
// Capture performs `git add -A` first so untracked files are included
// in the stash, then `git reset` to un-stage so the caller's index is
// left unmodified (same non-mutating contract as CreateCheckpoint).
//
// Records a "tree.captured" declaration in the scope's trace. The trace
// record is advisory: a failure to append it does not fail the capture.
func (s *Scope) CaptureTree(repoPath string) (*TreeState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.state != ScopeActive {
		return nil, fmt.Errorf("cannot capture tree in scope %s state %s", s.id, s.state)
	}

	g := &gitRunner{repoPath: repoPath}

	if !g.isRepo() {
		return nil, fmt.Errorf("capture tree: %s is not a git repository", repoPath)
	}

	if err := g.stageAll(); err != nil {
		return nil, fmt.Errorf("capture tree: git add -A: %w", err)
	}

	stashSHA, err := g.stashCreate()
	if err != nil {
		return nil, fmt.Errorf("capture tree: git stash create: %w", err)
	}

	// Un-stage so the capture is non-mutating w.r.t. the caller's index.
	if err := g.unstageAll(); err != nil {
		return nil, fmt.Errorf("capture tree: git reset: %w", err)
	}

	headSHA, err := g.headSHA()
	if err != nil {
		return nil, fmt.Errorf("capture tree: git rev-parse HEAD: %w", err)
	}

	// Record in trace (advisory — log and continue on failure). The
	// intent ID carries the monotonic tree sequence so recapturing the
	// same HEAD with different content (e.g. fork flows) is a distinct
	// intent, not a collision.
	treeSeq := nextCheckpointSeq.Add(1)
	_, err = s.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:tree:capture:%d", s.ownerID, treeSeq),
		Groups: []AppendGroup{{
			TraceOwnerID: s.ownerID,
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: SchemaTreeCaptured,
				KindLabel: "tree:captured",
				Payload: map[string]any{
					"head_sha":  headSHA,
					"stash_sha": stashSHA,
				},
			}},
		}},
	})
	if err != nil {
		slog.Warn("capture tree: record in trace failed (advisory)", "err", err)
	}

	return &TreeState{HeadSHA: headSHA, StashSHA: stashSHA}, nil
}

// ApplyTree resets the workspace of repoPath to the given TreeState.
//
// The git sequence mirrors RestoreCheckpoint — `git reset --hard
// <headSHA>`, `git clean -fd`, then `git stash apply <stashSHA>` when
// the captured tree was dirty — but consumes nothing: the TreeState
// stays valid and can be applied again later.
//
// Records a "tree.applied" capture in the scope's trace (advisory).
func (s *Scope) ApplyTree(repoPath string, ts *TreeState) error {
	if ts == nil {
		return fmt.Errorf("apply tree: nil TreeState")
	}

	s.mu.RLock()
	scopeActive := s.state == ScopeActive
	s.mu.RUnlock()

	if !scopeActive {
		return fmt.Errorf("cannot apply tree in scope %s state %s", s.id, s.state)
	}

	if err := applyTreeState(repoPath, ts); err != nil {
		return err
	}

	applySeq := nextCheckpointSeq.Add(1)
	_, err := s.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:tree:apply:%d", s.ownerID, applySeq),
		Groups: []AppendGroup{{
			TraceOwnerID: s.ownerID,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaTreeApplied,
				KindLabel: "tree:applied",
				Payload: map[string]any{
					"head_sha":  ts.HeadSHA,
					"stash_sha": ts.StashSHA,
				},
			}},
		}},
	})
	if err != nil {
		slog.Warn("apply tree: record in trace failed (advisory)", "err", err)
	}

	return nil
}

// applyTreeState runs the destructive git sequence shared by
// RestoreCheckpoint and ApplyTree. It is a free function because it
// touches no scope or checkpoint state.
func applyTreeState(repoPath string, ts *TreeState) error {
	g := &gitRunner{repoPath: repoPath}

	if err := g.resetHardTo(ts.HeadSHA); err != nil {
		return fmt.Errorf("apply tree: git reset --hard %s: %w", ts.HeadSHA, err)
	}

	if err := g.cleanFiles(); err != nil {
		return fmt.Errorf("apply tree: git clean -fd: %w", err)
	}

	if ts.StashSHA != "" {
		if err := g.stashApply(ts.StashSHA); err != nil {
			return fmt.Errorf("apply tree: git stash apply %s: %w", ts.StashSHA, err)
		}
	}

	return nil
}

// DiffSince returns the workspace changes relative to headSHA as a
// unified diff plus the list of changed file paths.
//
// It stages everything first (`git add -A`) so untracked files appear
// in the diff — the same staging behavior as CreateCheckpoint and
// CaptureTree, which the callers run beforehand anyway.
//
// maxLines bounds the returned diff (0 = unbounded). When the diff is
// truncated, a "...[diff truncated]" marker line is appended; the files
// list is never truncated.
//
// DiffSince is a package-level query: it records no trace event and
// needs no scope.
func DiffSince(repoPath, headSHA string, maxLines int) (diff string, files []string, err error) {
	g := &gitRunner{repoPath: repoPath}

	if !g.isRepo() {
		return "", nil, fmt.Errorf("diff since: %s is not a git repository", repoPath)
	}

	if err := g.stageAll(); err != nil {
		return "", nil, fmt.Errorf("diff since: git add -A: %w", err)
	}

	names, err := g.run("diff", "--cached", "--name-only", headSHA)
	if err != nil {
		return "", nil, fmt.Errorf("diff since: git diff --name-only: %w", err)
	}
	if names != "" {
		files = strings.Split(names, "\n")
	}

	full, err := g.run("diff", "--cached", headSHA)
	if err != nil {
		return "", nil, fmt.Errorf("diff since: git diff: %w", err)
	}

	if maxLines > 0 {
		lines := strings.Split(full, "\n")
		if len(lines) > maxLines {
			full = strings.Join(lines[:maxLines], "\n") + "\n...[diff truncated]"
		}
	}

	return full, files, nil
}
