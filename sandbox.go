package shepherd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
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
// earlier git-specific checkpoint fields so any backend can be captured and
// restored through one type.
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

// DeltaApplier is an optional Sandbox capability: applying a captured
// state's changes onto a workspace that has moved past the state's base,
// without resetting it. Sandbox.Apply is a full reset and is wrong for that
// case — it would roll the workspace back to the state's HEAD. Settlement's
// apply verb (plan 04 §2) needs the merge instead.
//
// A delta state records its own base (the git backend's stash commit carries
// its parent), so one argument is enough: the backend merges the delta onto
// whatever the workspace holds now. A conflict fails the call and must
// leave the workspace untouched.
//
// Backends that cannot merge a delta return ErrUnsupported from the
// assertion's absence — callers detect the capability with a type
// assertion, the same pattern the containerd adapter's StateReleaser uses.
// Keeping this off the Sandbox interface means adding it is a backend
// decision, not a kernel-wide breaking change.
type DeltaApplier interface {
	// ApplyDelta merges the changes captured in delta onto the current
	// workspace. A delta with no changes (a clean capture) is a no-op.
	// A conflicting delta returns an error and consumes nothing.
	ApplyDelta(ctx context.Context, delta WorkspaceState) error
}

// --- Sandbox backend registry ---

// ErrUnknownSandboxBackend is the sentinel behind UnknownSandboxBackendError,
// so callers can errors.Is(err, ErrUnknownSandboxBackend) without matching
// strings.
var ErrUnknownSandboxBackend = errors.New("shepherd: unknown sandbox backend")

// UnknownSandboxBackendError names the backend that was asked for and the
// backends that are actually registered, so a config typo fails with the
// valid options instead of a bare "not found".
type UnknownSandboxBackendError struct {
	Backend   string
	Available []string
}

func (e *UnknownSandboxBackendError) Error() string {
	return fmt.Sprintf("shepherd: unknown sandbox backend %q (available: %s)",
		e.Backend, strings.Join(e.Available, ", "))
}

func (e *UnknownSandboxBackendError) Is(target error) bool { return target == ErrUnknownSandboxBackend }

// SandboxFactory builds a Sandbox from backend-defined configuration for
// config-driven backend selection: a host resolves a configured backend name
// through OpenSandbox instead of importing each backend's constructor.
//
// cfg is opaque to the kernel — each backend documents the concrete type it
// accepts (the built-in git backend takes GitSandboxConfig) and rejects
// anything else loudly, so a configuration mismatch is a startup error, not
// a silent default.
//
// The returned Sandbox is NOT provisioned: Create remains a separate,
// explicit lifecycle step, exactly as it is for direct constructor use.
type SandboxFactory func(cfg any) (Sandbox, error)

// sandboxRegistry is the instance behind the package-level functions, so the
// registration logic is testable without mutating the global. Registration
// is expected from init() or host wiring — a duplicate, an empty name, or a
// nil factory is a programming error and panics (the database/sql driver
// convention), unlike substrate registration whose runtime inputs get
// errors.
type sandboxRegistry struct {
	mu        sync.Mutex
	factories map[string]SandboxFactory
}

func (r *sandboxRegistry) register(backend string, f SandboxFactory) {
	if backend == "" {
		panic("shepherd: RegisterSandbox called with an empty backend name")
	}
	if f == nil {
		panic(fmt.Sprintf("shepherd: RegisterSandbox called with a nil factory for %q", backend))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[backend]; dup {
		panic(fmt.Sprintf("shepherd: sandbox backend %q is already registered", backend))
	}
	r.factories[backend] = f
}

func (r *sandboxRegistry) open(backend string, cfg any) (Sandbox, error) {
	r.mu.Lock()
	f, ok := r.factories[backend]
	r.mu.Unlock()
	if !ok {
		return nil, &UnknownSandboxBackendError{Backend: backend, Available: r.backends()}
	}
	return f(cfg)
}

func (r *sandboxRegistry) backends() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// defaultSandboxRegistry is the package-level registry. The built-in git
// backend self-registers from sandbox_git.go; the containerd adapter
// registers when the host imports it — a host that never imports a backend
// cannot select it, which keeps the dependency tree honest: config can name
// a backend, only imports can provide one.
var defaultSandboxRegistry = &sandboxRegistry{factories: map[string]SandboxFactory{}}

// RegisterSandbox registers a backend for OpenSandbox. Intended for
// init()-time self-registration (the built-in git backend) and host wiring
// (importing a backend module and registering it under its Backend() name).
func RegisterSandbox(backend string, f SandboxFactory) {
	defaultSandboxRegistry.register(backend, f)
}

// OpenSandbox constructs the named backend's Sandbox from the backend's own
// configuration type. The sandbox is not provisioned — call Create as the
// lifecycle contract requires.
func OpenSandbox(backend string, cfg any) (Sandbox, error) {
	return defaultSandboxRegistry.open(backend, cfg)
}

// SandboxBackends lists the registered backend names, sorted, for config
// diagnostics and doctor output.
func SandboxBackends() []string {
	return defaultSandboxRegistry.backends()
}
