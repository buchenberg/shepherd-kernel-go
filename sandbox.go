package shepherd

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// ErrUnsupported is returned by a Sandbox method whose capability is false
// according to Capabilities. Callers should check Capabilities before calling
// an optional method rather than relying on the error, but backends return it
// so a misconfigured caller fails loudly instead of silently doing nothing.
var ErrUnsupported = errors.New("shepherd: sandbox capability not supported")

// ErrNoSandbox is returned when a workspace or checkpoint operation is
// attempted on a scope that was created without a sandbox. Pure-causal scopes
// (no filesystem) are valid; only workspace operations are rejected.
var ErrNoSandbox = errors.New("shepherd: scope has no sandbox")

// SandboxCapabilities declares what a backend supports and how its workspace
// is contained. Callers use it to decide whether an optional operation is
// available before invoking it.
type SandboxCapabilities struct {
	// Lifecycle reports whether Create and Destroy manage real resources.
	// A backend that only validates or materializes reports false.
	Lifecycle bool
	// Exec reports whether Exec is implemented.
	Exec bool
	// FileIO reports whether ReadFile and WriteFile are implemented.
	FileIO bool
	// Diff reports whether Diff is implemented.
	Diff bool
	// Isolated reports whether the workspace is isolated from the host
	// filesystem. A false value means the backend mutates the host tree
	// in place.
	Isolated bool
	// Containment is the witness-level containment classification for the
	// substrate, stamped into trace witnesses and retained contexts.
	Containment Containment
}

// WorkspaceState is a backend-neutral snapshot of a workspace. It replaces the
// git-specific pair (GitCheckpoint.StashSHA/HeadSHA and TreeState) so any
// backend can be captured and restored through one type.
//
// Data is backend-specific and opaque to the kernel. It uses map[string]any to
// match RecordDraft.Payload and to be directly digestible by CanonicalDigest
// without a JSON round-trip. A backend carrying opaque binary state must
// base64-encode it into a string.
//
// A WorkspaceState is inherently reusable: Apply consumes nothing, so the same
// value can be applied any number of times.
type WorkspaceState struct {
	// Backend identifies the backend that produced the state ("git",
	// "containerd", ...). Apply rejects a state from a different backend.
	Backend string
	// Revision is an optional backend timeline position: the git HEAD SHA for
	// the git backend, a snapshot key for a snapshot-backed backend.
	Revision string
	// Data carries the backend-specific payload.
	Data map[string]any
}

// Digest returns the kernel canonical digest of the state, for trace records
// and drift detection. It is deterministic: CanonicalDigest sorts keys at every
// nesting level, so two states with the same content digest identically
// regardless of map iteration order.
//
// It returns an error only if Data contains a value that cannot be canonically
// marshaled. Callers recording a trace treat a digest failure as advisory.
func (ws WorkspaceState) Digest() (string, error) {
	return CanonicalDigest(map[string]any{
		"backend":  ws.Backend,
		"revision": ws.Revision,
		"data":     ws.Data,
	})
}

// SandboxSpec carries per-call runtime options for Create. Backend-specific
// configuration (repository path, base image, credentials) belongs on the
// backend constructor, not here.
type SandboxSpec struct {
	// Workdir is the workspace path for backends that mount or check out into
	// a caller-chosen directory. Ignored by backends that infer their own.
	Workdir string
	// Env is the environment for commands run during Create (for example a
	// bootstrap step). It is not the agent's execution environment.
	Env map[string]string
	// Timeout bounds the Create operation. Zero means the backend default.
	Timeout time.Duration
}

// ExecRequest describes one command execution inside a sandbox.
//
// The shape mirrors the Daytona toolbox request
// (apps/daemon/pkg/toolbox/process/types.go) so a future HTTP backend is a
// straight mapping rather than an adapter.
type ExecRequest struct {
	Command string
	Args    []string
	Cwd     string
	Env     map[string]string
	Timeout time.Duration
	Stdin   []byte
}

// ExecResult is the outcome of one command execution.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Sandbox is the execution substrate for a scope. It owns a workspace and can
// provision it, snapshot it, restore it, diff it, and execute in it.
//
// Backends need not implement every method: Capabilities reports which are
// available and unsupported methods return ErrUnsupported. This lets the git
// backend, which is a materializer rather than an isolation boundary, satisfy
// the interface without pretending to provide execution or isolation.
//
// Every method takes a context so remote backends can honor cancellation and
// deadlines. Local backends should still impose their own ceiling (the git
// backend bounds each subprocess with gitTimeout) so a caller passing
// context.Background() cannot hang indefinitely.
//
// Non-mutating contract: Capture and Diff must leave the caller's workspace
// metadata unchanged on return. For git that means the index must be left as
// it was found.
type Sandbox interface {
	// Backend returns the backend identifier ("git", "containerd", ...).
	Backend() string

	// Capabilities reports what this backend supports.
	Capabilities() SandboxCapabilities

	// Create provisions or validates the substrate. A materializer validates
	// that its target exists; an isolating backend allocates real resources.
	Create(ctx context.Context, spec SandboxSpec) error

	// Destroy releases resources this backend provisioned. It must never
	// destroy resources the backend did not create — a materializer pointed at
	// a user's repository must not delete it.
	Destroy(ctx context.Context) error

	// Capture records the current workspace state. Non-mutating.
	Capture(ctx context.Context) (WorkspaceState, error)

	// Apply resets the workspace to a previously captured state. The git
	// backend's state is reusable; a snapshot-backed backend may instead
	// provision fresh resources and rebind its identity, making Apply
	// single-use per state. Callers must not assume reuse unless
	// Capabilities documents it.
	Apply(ctx context.Context, ws WorkspaceState) error

	// Diff returns the unified diff and changed file paths between ws and the
	// current workspace. maxLines <= 0 means unbounded. Non-mutating.
	Diff(ctx context.Context, ws WorkspaceState, maxLines int) (diff string, files []string, err error)

	// Exec runs a command in the sandbox and returns its result.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// ReadFile reads a file inside the sandbox workspace.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes a file inside the sandbox workspace.
	WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}
