# Changelog

Notable changes to `shepherd-kernel-go`. This project follows
[Semantic Versioning](https://semver.org/); while pre-1.0, minor releases may
contain breaking changes, which are called out below.

## [v0.4.1] - 2026-10-07

Patch release. **`v0.4.0` does not contain these fixes**: it was tagged at the
`feat/containerd` merge (`0c62788`) before the Phase 0 bug batch landed, so the
pre-fix code was the only thing a `go get` could resolve. `v0.4.1` supersedes it.

### Fixed
- `HighErrorRateRule` denominator computed over the wrong population.
- `ExternalAnchor.AnchorKind` left unpopulated by an argument-position bug.
- Scope-event intent IDs now use an atomic sequence, so concurrent scope events
  cannot collide.
- `ExecResult.Duration` is populated by the containerd `Exec` path.
- Sub-agents no longer open a second trace store; middleware built on the
  session-shared store no longer closes it.
- Stale `TreeState`/`DiffSince` comments removed.

### Added
- `sandbox/containerd/live_test.go`: an env-gated live harness
  (`SHEPHERD_CONTAINERD_ADDR`, `_IMAGE`, `_NAMESPACE`, `_SNAPSHOTTER`,
  `_WORKDIR`, `_TIMEOUT`) covering full lifecycle, namespace isolation, destroy
  keeping the image rootfs, diff preserving pre-staged changes, and stdin
  rejection. Skips cleanly without a daemon.
- `Config.FIFODir` and `Config.WriteChunkBytes` on the containerd sandbox, so an
  unprivileged caller can drive the backend without root-owned defaults.

### Fixed (containerd, found by first live-daemon exercise)
- `RootfsSnapshot` used the image's manifest digest as the snapshot parent key.
  containerd's unpacker commits each layer under its chainID, so `Prepare` failed
  with "parent snapshot ... does not exist".
- `Exec` never called `proc.Start`, so the process stayed created and every exec
  blocked until the context expired.
- `StartTask` used `cio.WithStdio`, tying the container's stdin to the client's.
- `StopTask` killed only the init process, orphaning exec'd processes.
- A cancelled context surfaced as exit 255 with a nil error.
- `Diff` mutated the caller's git index, silently dropping pre-staged work.
- `Exec` now rejects `ExecRequest.Stdin` explicitly (`ErrStdinUnsupported`): the
  runc-v2 shim holds a write end on the exec's stdin FIFO, so a reader never sees
  EOF and a stdin-reading command hangs rather than erroring.

### Known limitation — containerd backend is not yet reliable
The live harness is **flaky**: 3 of 5 consecutive runs failed, with a different
test failing each time. The adapter holds **no containerd lease**, so once
`Capture` stops the task its just-committed snapshot is unreferenced and the
daemon's garbage collector can reap it before the successor is prepared (a
default install runs `mutation_threshold = 100`, `schedule_delay = 0s`).
Treat the containerd backend as **unverified** until a lease is held for the
sandbox's lifetime — tracked as T0.9 in `plans/00-execution-plan.md`. In a long
run it can lose its workspace mid-operation.

## [v0.4.0] - 2026-09-16

### Added
- Backend-neutral `Sandbox` interface with `WorkspaceState` capture, apply, and
  diff, implemented for git (in-place and worktree) and containerd.
- `sandbox/containerd`: a nested Linux module implementing the sandbox on
  containerd's overlayfs snapshotter (lifecycle, file I/O, exec, diff).
- `PARITY-PLAN.md` and a `plans/` directory describing the roadmap toward
  parity with the Python `shepherd` kernel.

### Changed
- `Checkpoint` carries backend-neutral `WorkspaceState` instead of git
  stash/HEAD fields; checkpoint trace schemas bumped to
  `shepherd.checkpoint.*.v2`.
- `Scope` gains explicit sandbox ownership (`WithSandbox`); `Fork` inherits the
  parent's sandbox without owning it. `ScopeManager.ForkIsolated` added.

### Breaking
- Removed `TreeState` and `tree.go`; use `Sandbox.Capture`/`Apply`/`Diff` with
  `WorkspaceState`.
- Renamed `GitCheckpoint` to `Checkpoint`; `ScopeManager.Create` now takes a
  `Sandbox`; checkpoint create/restore take a `context.Context` and no longer a
  repository path.

## [v0.3.2] - 2026-08-18

### Fixed
- Capture un-stages after `git add -A` so a checkpoint no longer mutates the
  caller's index.

## [v0.3.1] - 2026-08-17

### Fixed
- Tree capture/apply now derive unique trace intent IDs; the previous
  content-addressed IDs collided.

## [v0.3.0] - 2026-08-17

### Added
- `TreeState` capture/apply and `DiffSince` for reusable workspace snapshots.

## [v0.2.1] - 2026-08-12

### Fixed
- SQLite trace store enables WAL mode and a busy timeout.

## [v0.2.0] - 2026-08-12

### Added
- Checkpoint create/restore with single-use semantics.
- `InterventionDeny`, letting the supervisor block a tool call rather than only
  injecting guidance.

### Fixed
- Monotonic checkpoint identity and ordering (no wall-clock ties or
  collisions).

## [v0.1.1] - 2026-08-10

### Added
- Ring-0 kernel: canonical JSON v2 digests, a content-addressed SQLite trace
  store with witnesses, cuts/frontiers, and slices.
- `EffectBus` for real-time effect subscription.
- `Scope`/`ScopeManager` fork, merge, and discard, recorded in the trace.
- `Supervisor` rules engine with inject/halt interventions and built-in rules.
- Scope snapshots for speculative execution.

> `0.1.0` is an alias for the same commit as `v0.1.1`; `v0.1.1` is the first
> `v`-prefixed tag.
