# Plan 05 — Persistence, Hygiene & Release

**Phase**: 0 (items 1–3, 5 run **first**) + ongoing (items 4, 6–8) · **Priority**: P0 (process) · **Estimate**: ~1–2 weeks total, interruptible
**Goal**: Make the current state of the port real and consumable (v0.4.0
exists only on an untagged branch), fix the known small bugs in one batch,
close the durability gap (scopes/checkpoints die with the process), and set up
the CI guardrails that keep ABI parity (plan 01) from silently rotting.

---

## 1. Release reality check

Findings from the inventory (2026-09-16):

- `main` is at `a07ead4` = tag **v0.3.2**. All v0.4.0 content
  (backend-neutral `Sandbox`, `WorkspaceState`, containerd backend) lives only
  on `feat/containerd` (`48b16fb`), **untagged**. `go get
  github.com/buchenberg/shepherd-kernel-go` fetches ≤ v0.3.2 — i.e. the old
  `TreeState` API that GAP-REPORT calls superseded.
- `GAP-REPORT.md` and `sandbox/containerd/go.mod` both reference **v0.4.0**,
  which does not exist as a tag.
- **No LICENSE file** (README admits it). Python upstream ships one — check
  `../shepherd/LICENSE` and align (do not pick a license family unilaterally;
  if upstream is copyleft and this is intended MIT-compatible, that is a
  decision for the owner, flagged here).

**Work**:
1. Review `feat/containerd` diff vs `main` (two commits: `c4784d4`,
   `48b16fb`), merge to `main` (PR with the review checklist below).
2. Decide version semantics: the Sandbox refactor **removed** `TreeState`/
   `DiffSince` (breaking, pre-1.0) → **v0.4.0** is correct per the docs.
3. Tag `v0.4.0`; add `CHANGELOG.md` (v0.1.0→v0.4.0 reconstructed from tags +
   reflog; keep short — the story is: kernel → bus/scopes/supervisor →
   checkpoints → TreeState → Sandbox+containerd).
4. Add LICENSE.
5. Fix `sandbox/containerd/go.mod`: the `replace` directive must go **before**
   tagging the nested module (it points at `../..`, making the published module
   unbuildable — the file's own comment says so). Requires core v0.4.0 to be
   tagged first, then bump the nested `require` and tag the adapter (e.g.
   `sandbox/containerd/v0.1.0`).

## 2. Build constraints for the containerd module

- No `//go:build` line exists anywhere; Linux-only-ness is prose.
  `client.go` uses `syscall.SIGKILL` → the nested module **fails to compile on
  Windows/macOS** with an obscure error.
- Add `//go:build linux` to `client.go` (the daemon adapter) — keep
  `sandbox.go` (lifecycle logic, fully tested against fakes) portable so the
  invariant tests run on every platform. If any type referenced by
  `sandbox.go` comes from containerd client packages, isolate those behind the
  tag with a small `backend_linux.go` / `backend_other.go` stub pair
  (`ErrNotImplemented` off-Linux).
- Add CI compile check on linux (item 8) so the tagged module is provably
  buildable on its target platform.

## 3. containerd daemon verification (gated)

The daemon path (`client.go`: namespaces, image pull/unpack, task generations,
SIGKILL teardown) has **never executed** — 22 tests run against the fake
snapshotter only.

- Add `sandbox/containerd/live_test.go` behind
  `//go:build linux && !nolive` + env guard `SHEPHERD_CONTAINERD_ADDR`
  (skip unless set): create from a small public image → WriteFile → Capture →
  mutate → Apply → verify rollback → Exec → Diff → Destroy. This is the
  acceptance test for the whole backend.
- Run it once locally on a Linux box / WSL2 with containerd, record the
  outcome in README's status note (upgrade "unexercised" to "verified against
  containerd X.Y on <date>" or file the discovered bugs).

## 4. Durability: recover scopes, checkpoints, outputs from the trace

The parity gap: Python traces + vcs-core scope registry survive restart; Go
`ScopeManager` (scopes, checkpoints, plan-04 retained outputs) is in-memory —
a process restart strands worktree sandboxes and loses restorability even
though every event is durably recorded.

**Approach** (trace-first, minimal new persistence):
```go
// scope_manager.go
func RecoverScopes(store *SQLiteTraceStore, opts RecoverOptions) (*ScopeManager, error)
// Scan owner prefixes for scope lifecycle schema refs
// (shepherd.scope.{forked,merged,discarded}.v1, supervisor.halt) and rebuild
// the scope tree + terminal states. Sandboxes are NOT auto-reattached; opts
// carries a resolver func(scopeID, backend string) (Sandbox, error) so the
// host (yaah) decides which worktrees/containers to re-adopt vs orphan-clean.
```
- **Checkpoints**: metadata (ID, seq, scope, backend, revision, state_digest)
  is already in `shepherd.checkpoint.created.v2` records — recovery can
  rebuild the registry. The **opaque snapshot bytes** are not recorded
  (deliberately — they can be large). Options, decide at implementation:
  (a) persist to a SQLite blob table `checkpoint_snapshots(checkpoint_id,
  snapshot BLOB)` in the trace DB (simple, durable, grows the file);
  (b) sidecar file per checkpoint under a host-provided dir; (c) remain
  volatile and document restore-after-restart as workspace-only.
  Recommend (a) with a size guard + `PruneCheckpoints` deleting blobs.
- **Staleness on recovery**: a recovered checkpoint whose recorded
  `state_digest` no longer matches `Sandbox.Capture()` of the re-adopted
  sandbox is marked `CheckpointInvalid` (never silently restorable).
- Plan-04 outputs recover the same way (sealed/settled records carry
  state_digest + backend + revision).
- Also fix: merged/discarded scopes are never unregistered (unbounded map);
  keep them lookupable (terminal-state reads are useful) but add
  `PruneTerminal()` for long-lived hosts.

**Tests**: restart round-trip (create → fork → checkpoint → close → recover →
restore works); recovered checkpoint vs moved workspace → invalid; resolver
orphans cleaned; terminal states recovered correctly.

## 5. Bug & idiom batch (small, one PR)

| # | Fix | Where |
|---|---|---|
| 1 | `HighErrorRateRule` message denominator always 0 — `len(st.results)` formatted **after** `st.results = nil` | supervisor.go ~393 |
| 2 | `ExternalAnchor.AnchorKind` never populated (arg-position bug) — also plan 01 §2; land once | store.go ~1034/1098 |
| 3 | Scope-event intent IDs use `time.Now().UnixNano()` (non-monotonic, collision-prone under fast retries) — switch to the existing `nextCheckpointSeq` atomic | scope.go |
| 4 | `ExecResult.Duration` never populated — set it in containerd `Exec` (git backend stays `ErrUnsupported`) | sandbox/containerd |
| 5 | Stale comments referencing removed `TreeState`/`DiffSince` | sandbox.go:44, workspace_test.go:282 |
| 6 | `git stash create` after `git add -A` then `git reset` — verify behavior with pre-staged caller index (already fixed once in `a07ead4`; add a regression test for "caller had staged changes before Capture") | sandbox_git.go |

## 6. `context.Context` in store APIs (Go idiom, pre-1.0 window)

Store methods take only trace-auth contexts; there is no cancellation or
deadline anywhere in the read/append path. Before consumers (yaah) hard-code
around this:

- Add `ctx context.Context` as first parameter to `Append`, `Preview*`,
  `Read*`, `Resolve*`, `Publish*`, `Close` (keep trace-auth types as the
  second parameter — they are the ABI-facing authority, not a Go ctx).
- Internal `database/sql` calls switch to the `Context` variants; the
  `gitRunner`/sandbox boundary already threads ctx.
- This is a breaking API change → bundle into the **v0.5.0** release after
  plans 01–02 land (or earlier if yaah coordination is trivial; yaah's calls
  are mechanical).

## 7. README / docs refresh (per landed plan)

- Add `CheckCall` + `DestructiveToolGuard` to the supervision example
  (currently API-section-only).
- Document: no `context.Context` (until §6), in-memory checkpoints (until §4),
  containerd module framing (nested, Linux-gated — the architecture box
  currently implies both backends ship in-module).
- After v0.4.0 tag: replace "161 tests" style counts with a generated number
  or drop exact counts (they rot).
- PARITY-PLAN status table gets a ✅ per landed item (keep this doc honest the
  way GAP-REPORT's resolution table was).

## 8. CI (new — repo has no workflows)

Minimal GitHub Actions (`ci.yml`):
- matrix: `ubuntu-latest`, `windows-latest`, `macos-latest` × `go test ./... -count=1 -race` (core module; git-backed tests self-skip without git, CI has git).
- `go vet ./...`, `gofmt -l .` (fail on output).
- Nested containerd module: linux-only job — `go build ./...` + `go test ./...`
  (fakes run anywhere the module compiles; live daemon test stays env-gated).
- **Golden-vector drift check**: job clones `../shepherd` equivalent
  (github.com/shepherd-agents/shepherd at a pinned tag), hashes
  `shepherd2/tests/golden/kernel_abi_v0.json` and compares against
  `testdata/kernel_abi_v0.json` — mismatch fails with instructions from
  plan 01 §3.
- Release job on tag: `gorelease`-style verification (module builds from a
  clean checkout, no replace directives in tagged modules).

## Acceptance criteria

- [ ] `v0.4.0` tagged on merged `main`; nested containerd module tagged
      without `replace`; `go get` resolves both.
- [ ] LICENSE present; CHANGELOG covers v0.1.0–v0.4.0.
- [ ] Core module compiles/tests on all three OSes in CI; containerd compiles
      on linux, politely `ErrNotImplemented` elsewhere where applicable.
- [ ] Live containerd smoke test passed once on a real daemon (result noted
      in README).
- [ ] Recover-after-restart test suite green; staleness detection works.
- [ ] Bug batch (§5) merged; golden-drift CI check active.
