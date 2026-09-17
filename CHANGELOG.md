# Changelog

Notable changes to `shepherd-kernel-go`. This project follows
[Semantic Versioning](https://semver.org/); while pre-1.0, minor releases may
contain breaking changes, which are called out below.

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
