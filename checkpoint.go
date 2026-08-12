package shepherd

import (
	"bytes"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Schema refs for checkpoint trace recording.
const (
	SchemaCheckpointCreated  = "shepherd.checkpoint.created.v1"
	SchemaCheckpointRestored = "shepherd.checkpoint.restored.v1"
)

// CheckpointState tracks whether a checkpoint is usable.
type CheckpointState string

const (
	CheckpointValid   CheckpointState = "valid"
	CheckpointUsed    CheckpointState = "used"    // restored already
	CheckpointStale   CheckpointState = "stale"   // HEAD moved underneath us
	CheckpointInvalid CheckpointState = "invalid" // git operations failed
)

// GitCheckpoint captures workspace + conversation state at a point in time.
//
// The workspace state is a git stash SHA (or HEAD ref if the tree was clean).
// The conversation state is an opaque []byte the caller provides — typically
// JSON-marshaled conversation history from the agent loop.
//
// Checkpoints are single-use: RestoreCheckpoint marks them as "used".
// A stale checkpoint (HEAD moved after creation without a restore) is detected
// via HEAD comparison.
type GitCheckpoint struct {
	// ID is a unique checkpoint identifier (auto-generated).
	ID string
	// ScopeID is the scope this checkpoint belongs to.
	ScopeID string
	// RepoPath is the git repo path, stored at creation so RestoreCheckpoint
	// doesn't need the caller to pass it again.
	RepoPath string
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

// gitRunner executes git commands in a repo directory.
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
	out, err := g.run("stash", "create")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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

// resetHardTo runs `git reset --hard <sha>` — resets to a specific commit.
// Used by RestoreCheckpoint to go back to the checkpoint's HEAD.
func (g *gitRunner) resetHardTo(sha string) error {
	_, err := g.run("reset", "--hard", sha)
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

// CreateCheckpoint captures the current workspace state and caller snapshot.
//
// Workspace state is captured via git:
//  1. `git add -A` — stage everything (including untracked files)
//  2. `git stash create` — create a stash commit (returns SHA, or empty if clean)
//  3. `git rev-parse HEAD` — record HEAD for staleness detection
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
		RepoPath:  repoPath,
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

// RestoreCheckpoint reverts the workspace to the checkpointed state and
// returns the stored conversation snapshot.
//
// Workspace restore via git:
//  1. `git reset --hard` — discard all tracked changes since checkpoint
//  2. `git clean -fd` — remove untracked files/dirs created since checkpoint
//  3. `git stash apply <sha>` — re-apply the checkpointed changes (if any)
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

	g := &gitRunner{repoPath: cp.RepoPath}

	// Step 1: Wipe workspace back to checkpoint's HEAD.
	// We reset to the checkpoint's HeadSHA, not current HEAD — this
	// correctly handles the case where new commits were made after
	// the checkpoint was taken.
	if err := g.resetHardTo(cp.HeadSHA); err != nil {
		cp.State = CheckpointInvalid
		return nil, fmt.Errorf("restore: git reset --hard %s: %w", cp.HeadSHA, err)
	}

	// Step 2: Remove untracked files
	if err := g.cleanFiles(); err != nil {
		cp.State = CheckpointInvalid
		return nil, fmt.Errorf("restore: git clean -fd: %w", err)
	}

	// Step 3: Re-apply checkpointed changes (if any).
	// The stash captures uncommitted changes at checkpoint time.
	// After reset --hard to HeadSHA, the tree is clean, so stash apply
	// restores the working-tree state as it was at checkpoint.
	if cp.StashSHA != "" {
		if err := g.stashApply(cp.StashSHA); err != nil {
			cp.State = CheckpointInvalid
			return nil, fmt.Errorf("restore: git stash apply %s: %w", cp.StashSHA, err)
		}
	}

	// No staleness warning needed — we explicitly reset to the checkpoint's
	// HEAD, so we're always at the right base.
	currentHead, _ := g.headSHA()

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
