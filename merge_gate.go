package shepherd

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Merge gate schema refs for trace recording (plan 04 §1).
const (
	// SchemaMergeProposed is recorded on the child's owner path when a merge
	// is proposed for review, before the reviewer runs.
	SchemaMergeProposed = "shepherd.merge.proposed.v1"
	// SchemaSupervisorDecision records the review outcome on the parent's
	// owner path, citing the proposed record as its causal parent.
	SchemaSupervisorDecision = "shepherd.supervisor.decision.v1"
)

// DefaultProposalDiffLines bounds the diff a CommitMerge proposal carries
// when the caller has not chosen a bound. It is deliberately generous: the
// reviewer sees the change, and the trace only stores the digest.
const DefaultProposalDiffLines = 1000

// Proposed change kinds.
const (
	ChangeCreate = "create"
	ChangeModify = "modify"
	ChangeDelete = "delete"
)

// ProposedChange is one workspace change a merge would introduce.
type ProposedChange struct {
	// Path is the workspace-relative path of the changed file.
	Path string
	// Kind is "create", "modify", or "delete".
	Kind string
	// Diff is the unified diff hunk for a modify or delete; the added
	// content (already bounded by the proposal's line limit) for a create.
	Diff string
}

// MergeProposal is what a reviewer sees: the child scope, the baseline the
// changes are against, and the changes themselves.
type MergeProposal struct {
	// ChildScopeID identifies the scope whose work is being merged.
	ChildScopeID string
	// BaselineDigest is the canonical digest of the parent's workspace state
	// captured at fork time — the state the diff is against.
	BaselineDigest string
	// Changes lists every file the merge would introduce, in diff order.
	Changes []ProposedChange
	// Diff is the full unified diff output (truncated to the proposal's
	// line limit).
	Diff string
}

// MergeReviewer decides whether a proposed merge may proceed. Approve by
// returning nil; deny by returning an error whose text becomes the denial
// reason recorded in the trace and carried on SupervisorDeniedError.
type MergeReviewer func(proposal MergeProposal) error

// SupervisorDeniedError is returned by CommitMerge when the reviewer denied
// the merge. The child scope has been discarded; the parent's workspace was
// never touched.
type SupervisorDeniedError struct {
	// Reason is the reviewer's error text.
	Reason string
	// Changes are the proposed changes that were denied.
	Changes []ProposedChange
}

func (e *SupervisorDeniedError) Error() string {
	if len(e.Changes) == 0 {
		return fmt.Sprintf("shepherd: merge review denied: %s", e.Reason)
	}
	paths := make([]string, 0, len(e.Changes))
	for _, c := range e.Changes {
		paths = append(paths, c.Path)
	}
	return fmt.Sprintf("shepherd: merge review denied: %s (changed paths: %s)",
		e.Reason, strings.Join(paths, ", "))
}

// ProposeMerge diffs the child's current workspace against the baseline
// captured at fork time and returns the proposal a reviewer would see,
// without running any review or changing any scope state. maxDiffLines <= 0
// means unbounded.
//
// The child must be active, have a sandbox with the Diff capability, and have
// been forked from a parent with a sandbox (the fork baseline). ProposeMerge
// is a query: it records no trace event and consumes nothing.
func (m *ScopeManager) ProposeMerge(ctx context.Context, childID string, maxDiffLines int) (*MergeProposal, error) {
	m.mu.RLock()
	child, ok := m.scopes[childID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("scope %s not found", childID)
	}

	if child.State() != ScopeActive {
		return nil, fmt.Errorf("cannot propose merge for scope %s in state %s", childID, child.State())
	}

	baseline, hasBaseline := child.Baseline()
	if !hasBaseline {
		return nil, fmt.Errorf("scope %s has no fork baseline: it was forked from a parent without a sandbox, so there is no state to diff against", childID)
	}

	sb := child.Sandbox()
	if sb == nil {
		return nil, fmt.Errorf("scope %s has no sandbox to diff in", childID)
	}
	if !sb.Capabilities().Diff {
		return nil, fmt.Errorf("scope %s sandbox backend %s cannot diff; merge review requires the Diff capability", childID, sb.Backend())
	}

	diff, files, err := child.DiffWorkspace(ctx, baseline, maxDiffLines)
	if err != nil {
		return nil, fmt.Errorf("propose merge for %s: %w", childID, err)
	}

	baselineDigest, err := baseline.Digest()
	if err != nil {
		return nil, fmt.Errorf("propose merge for %s: digest fork baseline: %w", childID, err)
	}

	return &MergeProposal{
		ChildScopeID:   child.ID(),
		BaselineDigest: baselineDigest,
		Changes:        classifyChanges(diff, files),
		Diff:           diff,
	}, nil
}

// CommitMerge runs the check-at-commit gate: propose, record, review, then
// merge or discard.
//
// The sequence is:
//
//  1. ProposeMerge builds the proposal from the child's current workspace
//     diffed against the fork baseline.
//  2. A shepherd.merge.proposed.v1 capture is recorded on the child's owner
//     path (change paths, kinds, count, and the diff digest — never the diff
//     body).
//  3. review(proposal) decides.
//  4. Approval records shepherd.supervisor.decision.v1 {approved: true} on
//     the parent's path and merges the child (the plain, ungated Merge).
//     Denial records the decision with the reason, discards the child —
//     never merges — and returns *SupervisorDeniedError.
//
// Merge is causal-only: it never rewrites the parent's workspace. CommitMerge
// is therefore for children that share the parent's sandbox — their work is
// already in the parent's tree, and a denial's rollback is the caller's
// checkpoint restore. A child with an isolated sandbox (a worktree) is
// refused with an error pointing at Seal + SettleSelect: merging it would
// causally record an approval and then destroy the worktree, losing the work
// the reviewer just approved. Settlement owns physical propagation for
// isolated children.
//
// A nil reviewer is rejected: skipping review is what plain Merge is for,
// and an accidental nil must not become an accidental approve.
func (m *ScopeManager) CommitMerge(ctx context.Context, childID string, review MergeReviewer) error {
	if review == nil {
		return fmt.Errorf("CommitMerge requires a reviewer; to skip review deliberately, call Merge")
	}

	m.mu.RLock()
	child, ok := m.scopes[childID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("scope %s not found", childID)
	}

	if sb := child.Sandbox(); sb != nil && sb.Capabilities().Isolated {
		return fmt.Errorf("CommitMerge refuses scope %s: its %s sandbox is isolated, so Merge would destroy the work the reviewer approved; seal the scope and SettleSelect its output instead", childID, sb.Backend())
	}

	parent := child.Parent()
	if parent == nil {
		return fmt.Errorf("scope %s has no parent (root scope)", childID)
	}

	proposal, err := m.ProposeMerge(ctx, childID, DefaultProposalDiffLines)
	if err != nil {
		return fmt.Errorf("commit merge for %s: %w", childID, err)
	}

	proposedFactID, err := recordMergeProposed(ctx, child, proposal)
	if err != nil {
		return err
	}

	reason := review(*proposal)
	if reason != nil {
		denyErr := &SupervisorDeniedError{Reason: reason.Error(), Changes: proposal.Changes}
		if err := recordSupervisorDecision(ctx, parent, proposal, false, reason.Error(), proposedFactID); err != nil {
			return errors.Join(denyErr, err)
		}
		// Deny → Discard, never Merge. The child's sandbox, if it owns one,
		// is destroyed by Discard, which is what makes a denial a physical
		// rollback for an isolating backend.
		if err := parent.Discard(child); err != nil {
			return errors.Join(denyErr, fmt.Errorf("discard denied scope %s: %w", childID, err))
		}
		return denyErr
	}

	if err := recordSupervisorDecision(ctx, parent, proposal, true, "", proposedFactID); err != nil {
		return err
	}
	return parent.Merge(child)
}

// recordMergeProposed appends the proposal record on the child's owner path.
// The append is hard, not advisory: it precedes the review decision, so a
// trace that cannot carry the proposal must not carry a decision about work
// that was never recorded as proposed.
func recordMergeProposed(ctx context.Context, child *Scope, proposal *MergeProposal) (string, error) {
	child.mu.RLock()
	ownerID := child.ownerID
	forkPoint := child.forkPoint
	child.mu.RUnlock()

	changes := make([]map[string]any, 0, len(proposal.Changes))
	for _, c := range proposal.Changes {
		changes = append(changes, map[string]any{"path": c.Path, "kind": c.Kind})
	}
	diffDigest, err := CanonicalDigest(map[string]any{"diff": proposal.Diff})
	if err != nil {
		diffDigest = ""
	}

	causedBy := []string{}
	if forkPoint != "" {
		causedBy = []string{forkPoint}
	}

	receipt, err := child.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:merge-proposed:%d", ownerID, nextCheckpointSeq.Add(1)),
		Groups: []AppendGroup{{
			TraceOwnerID:  ownerID,
			CausalParents: causedBy,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaMergeProposed,
				KindLabel: "merge:proposed",
				Payload: map[string]any{
					"child_scope":     proposal.ChildScopeID,
					"baseline_digest": proposal.BaselineDigest,
					"change_count":    len(proposal.Changes),
					"changes":         changes,
					"diff_digest":     diffDigest,
				},
			}},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("record merge proposal: %w", err)
	}
	if len(receipt.FactIDs) == 0 {
		return "", fmt.Errorf("record merge proposal: no fact ID returned")
	}
	return receipt.FactIDs[0], nil
}

// recordSupervisorDecision appends the review outcome on the parent's owner
// path, citing the proposal record. Like the proposal record, the append is
// hard: it precedes the physical merge or discard it authorizes.
func recordSupervisorDecision(
	ctx context.Context,
	parent *Scope,
	proposal *MergeProposal,
	approved bool,
	reason string,
	causedBy string,
) error {
	parent.mu.RLock()
	ownerID := parent.ownerID
	parent.mu.RUnlock()

	payload := map[string]any{
		"approved":    approved,
		"child_scope": proposal.ChildScopeID,
	}
	if !approved && reason != "" {
		payload["reason"] = reason
	}

	causalParents := []string{}
	if causedBy != "" {
		causalParents = []string{causedBy}
	}

	_, err := parent.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:merge-decision:%d", ownerID, nextCheckpointSeq.Add(1)),
		Groups: []AppendGroup{{
			TraceOwnerID:  ownerID,
			CausalParents: causalParents,
			FactDrafts: []RecordDraft{{
				Mode:      Capture,
				SchemaRef: SchemaSupervisorDecision,
				KindLabel: "supervisor:decision",
				Payload:   payload,
			}},
		}},
	})
	if err != nil {
		return fmt.Errorf("record supervisor decision: %w", err)
	}
	return nil
}

// classifyChanges turns a unified diff plus its changed-file list into the
// per-file proposed changes a reviewer sees. The diff is authoritative: a
// file's kind comes from its ---/+++ header pair (/dev/null on the old side
// is a create, on the new side a delete). Files that appear in the list but
// have no diff section — possible only for a backend emitting shapes the
// parser does not recognize — are included as modifies with an empty diff
// rather than silently dropped from review.
func classifyChanges(diff string, files []string) []ProposedChange {
	sections := parseUnifiedDiff(diff)

	seen := make(map[string]bool, len(sections))
	changes := make([]ProposedChange, 0, len(sections))
	for _, sec := range sections {
		p := sec.newPath
		// A deleted file's new side is /dev/null; the change is about the
		// old path.
		if p == "" || p == "/dev/null" {
			p = sec.oldPath
		}
		if p == "" || p == "/dev/null" || seen[p] {
			continue
		}
		seen[p] = true
		changes = append(changes, sec.change(p))
	}

	for _, f := range files {
		if !seen[f] {
			seen[f] = true
			changes = append(changes, ProposedChange{Path: f, Kind: ChangeModify})
		}
	}
	return changes
}

// diffSection is one file's portion of a unified diff.
type diffSection struct {
	oldPath string
	newPath string
	// lines are the section's raw lines, from the "diff --git" header
	// (or the ---/+++ pair, for backends without the git header) through the
	// last hunk line before the next section.
	lines []string
}

// change builds the ProposedChange for canonical path p.
func (s diffSection) change(p string) ProposedChange {
	kind := ChangeModify
	var body string
	switch {
	case s.oldPathIsDevNull():
		kind = ChangeCreate
		// Content preview for a create: the added lines.
		var added []string
		for _, l := range s.lines {
			if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
				added = append(added, strings.TrimPrefix(l, "+"))
			}
		}
		body = strings.Join(added, "\n")
	case s.newPathIsDevNull():
		kind = ChangeDelete
		body = strings.Join(s.lines, "\n")
	default:
		body = strings.Join(s.lines, "\n")
	}
	return ProposedChange{Path: p, Kind: kind, Diff: body}
}

func (s diffSection) oldPathIsDevNull() bool { return s.oldPath == "/dev/null" }
func (s diffSection) newPathIsDevNull() bool { return s.newPath == "/dev/null" }

// parseUnifiedDiff splits a unified diff into per-file sections, extracting
// the old and new path from each section's headers. The git header
// ("diff --git a/x b/x") starts a section; backends without it start one at
// a ---/+++ pair. Path prefixes "a/" and "b/" are stripped.
func parseUnifiedDiff(diff string) []diffSection {
	if strings.TrimSpace(diff) == "" {
		return nil
	}

	var sections []diffSection
	var cur *diffSection
	inHunk := false

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if cur != nil {
				sections = append(sections, *cur)
			}
			cur = &diffSection{lines: []string{line}}
			old, newp := parseDiffGitPaths(line)
			cur.oldPath, cur.newPath = old, newp
			inHunk = false
		case strings.HasPrefix(line, "--- "):
			if cur == nil {
				cur = &diffSection{}
			} else if inHunk {
				// A "---" inside a hunk body (deleted-line content that
				// itself starts with --- followed by a space): hunk lines
				// only reach here as content, so keep it in the body.
				cur.lines = append(cur.lines, line)
				continue
			}
			cur.lines = append(cur.lines, line)
			// The ---/+++ pair is authoritative for old/new presence — it
			// is how a unified diff says "no old side" (a create) or "no
			// new side" (a delete). The "diff --git" header names both
			// sides for every kind, so it cannot carry that signal and
			// must not win.
			cur.oldPath = stripDiffPath(strings.TrimSpace(strings.TrimPrefix(line, "--- ")))
		case strings.HasPrefix(line, "+++ "):
			if cur == nil {
				cur = &diffSection{}
				// A +++ without a preceding --- cannot be classified; treat
				// it as a malformed section and start collecting anyway.
				cur.lines = append(cur.lines, line)
				continue
			}
			if inHunk {
				cur.lines = append(cur.lines, line)
				continue
			}
			cur.lines = append(cur.lines, line)
			cur.newPath = stripDiffPath(strings.TrimSpace(strings.TrimPrefix(line, "+++ ")))
		default:
			if cur != nil {
				if strings.HasPrefix(line, "@@ ") {
					inHunk = true
				}
				cur.lines = append(cur.lines, line)
			}
		}
	}
	if cur != nil {
		sections = append(sections, *cur)
	}
	return sections
}

// parseDiffGitPaths extracts the old and new path from a
// "diff --git a/x b/x" header. Paths containing spaces defeat this naive
// split; git quotes such paths, and a quoted path simply yields no section —
// the file still appears via the changed-file list as an unclassified modify.
func parseDiffGitPaths(header string) (oldPath, newPath string) {
	rest := strings.TrimPrefix(header, "diff --git ")
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return stripDiffPath(parts[0]), stripDiffPath(parts[1])
}

// stripDiffPath removes the a/ or b/ prefix git adds to diff header paths.
func stripDiffPath(p string) string {
	if p == "/dev/null" {
		return p
	}
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return strings.TrimPrefix(strings.TrimPrefix(p, "a/"), "b/")
	}
	return p
}

// --- Built-in reviewers ---

// DraftsOnlyReviewer denies any change outside prefix — the Go analogue of
// the reference drafts_only_supervisor. prefix is a directory: "out" matches
// "out" and "out/anything", but not "outline.txt". An empty prefix approves
// everything (useful as a chain element, not as a gate).
func DraftsOnlyReviewer(prefix string) MergeReviewer {
	return func(p MergeProposal) error {
		for _, c := range p.Changes {
			if !pathWithin(c.Path, prefix) {
				return fmt.Errorf("change to %q is outside the drafts prefix %q", c.Path, prefix)
			}
		}
		return nil
	}
}

// pathWithin reports whether p is prefix itself or inside prefix/.
func pathWithin(p, prefix string) bool {
	if prefix == "" {
		return true
	}
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// DestructivePathReviewer denies any change touching a path that matches one
// of the given patterns. A pattern is matched against the workspace-relative
// path with path.Match semantics ("*.pem", "secrets/*.key"); a pattern
// ending in "/" matches everything beneath that directory ("secrets/").
// An empty pattern list denies nothing — an explicit list is an explicit
// contract, and a nil list should not silently become deny-everything.
func DestructivePathReviewer(patterns []string) MergeReviewer {
	return func(p MergeProposal) error {
		for _, c := range p.Changes {
			if pat := matchDestructivePath(c.Path, patterns); pat != "" {
				return fmt.Errorf("change to %q matches destructive-path pattern %q", c.Path, pat)
			}
		}
		return nil
	}
}

// matchDestructivePath returns the first pattern the path matches, or "".
// A pattern matches the full workspace-relative path or the file's base
// name — "*.pem" matches both "ca.pem" and "keys/ca.pem", because path.Match's
// * does not cross a separator and matching only the root would surprise
// every nested case.
func matchDestructivePath(p string, patterns []string) string {
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if strings.HasSuffix(pat, "/") {
			if strings.HasPrefix(p, pat) {
				return pat
			}
			continue
		}
		if ok, err := path.Match(pat, p); err == nil && ok {
			return pat
		}
		if ok, err := path.Match(pat, path.Base(p)); err == nil && ok {
			return pat
		}
	}
	return ""
}
