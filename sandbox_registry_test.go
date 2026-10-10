package shepherd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustPanic runs fn, failing the test unless it panicked with a message
// containing want.
func mustPanic(t *testing.T, want, label string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("%s: did not panic", label)
			return
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, want) {
			t.Errorf("%s: panic = %v, want it to mention %q", label, r, want)
		}
	}()
	fn()
}

func TestSandboxRegistry_InstanceRegistration(t *testing.T) {
	r := &sandboxRegistry{factories: map[string]SandboxFactory{}}

	// The programmer-error guards.
	mustPanic(t, "empty backend name", "empty name", func() { r.register("", func(any) (Sandbox, error) { return nil, nil }) })
	mustPanic(t, "nil factory", "nil factory", func() { r.register("x", nil) })
	r.register("x", func(any) (Sandbox, error) { return nil, nil })
	mustPanic(t, "already registered", "duplicate", func() { r.register("x", func(any) (Sandbox, error) { return nil, nil }) })

	// Unknown backends fail with the sentinel and list what exists.
	_, err := r.open("nope", nil)
	var unknown *UnknownSandboxBackendError
	if !errors.As(err, &unknown) || !errors.Is(err, ErrUnknownSandboxBackend) {
		t.Fatalf("open unknown: %v, want UnknownSandboxBackendError behind the sentinel", err)
	}
	if unknown.Backend != "nope" || len(unknown.Available) != 1 || unknown.Available[0] != "x" {
		t.Errorf("unknown error details: %+v", unknown)
	}
	if !strings.Contains(err.Error(), "x") {
		t.Errorf("error text %q should list the available backend", err.Error())
	}
}

func TestSandboxRegistry_GitIsRegistered(t *testing.T) {
	backends := SandboxBackends()
	found := false
	for _, b := range backends {
		if b == "git" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SandboxBackends() = %v, want the built-in git backend", backends)
	}
}

func TestSandboxRegistry_OpenGitInPlace(t *testing.T) {
	repo := newTestRepo(t)

	sb, err := OpenSandbox("git", GitSandboxConfig{RepoPath: repo})
	if err != nil {
		t.Fatalf("OpenSandbox (in-place): %v", err)
	}
	if sb.Backend() != "git" {
		t.Errorf("Backend() = %q, want git", sb.Backend())
	}
	caps := sb.Capabilities()
	if caps.Isolated || caps.Lifecycle {
		t.Errorf("in-place capabilities = %+v, want not isolated, no lifecycle", caps)
	}
	// In-place Create validates the repository and does not provision.
	if err := sb.Create(context.Background(), SandboxSpec{}); err != nil {
		t.Errorf("Create over the existing repo: %v", err)
	}
	if _, err := sb.Capture(context.Background()); err != nil {
		t.Errorf("Capture: %v", err)
	}
}

func TestSandboxRegistry_OpenGitWorktreeIsUnprovisioned(t *testing.T) {
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb, err := OpenSandbox("git", GitSandboxConfig{RepoPath: repo, WorktreePath: wtPath})
	if err != nil {
		t.Fatalf("OpenSandbox (worktree): %v", err)
	}
	if !sb.Capabilities().Isolated {
		t.Error("worktree sandbox must report Isolated")
	}
	// OpenSandbox never provisions: the worktree does not exist until
	// Create, which is the lifecycle contract.
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("OpenSandbox must not provision; worktree stat err = %v", statErr)
	}
	if err := sb.Create(context.Background(), SandboxSpec{}); err != nil {
		t.Fatalf("Create provisions the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("provisioned worktree should hold the repo's files: %v", err)
	}
	if err := sb.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestSandboxRegistry_ConfigValidation(t *testing.T) {
	if _, err := OpenSandbox("git", nil); err == nil || !strings.Contains(err.Error(), "GitSandboxConfig") {
		t.Errorf("nil config: %v, want it to demand a GitSandboxConfig", err)
	}
	if _, err := OpenSandbox("git", map[string]any{"repo": "x"}); err == nil || !strings.Contains(err.Error(), "map[") {
		t.Errorf("wrong config type: %v, want it to name the offending type", err)
	}
	if _, err := OpenSandbox("git", GitSandboxConfig{}); err == nil || !strings.Contains(err.Error(), "RepoPath is required") {
		t.Errorf("empty RepoPath: %v, want a required-field error", err)
	}
}
