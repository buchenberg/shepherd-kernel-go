# Shepherd ↔ shepherd-kernel-go Parity Plan

**Date**: 2026-09-16 · **Go branch**: `feat/containerd` (`48b16fb`, v0.4.0-content, untagged) · **Python reference**: `shepherd-workspace` 0.3.1 (main @ `d34d5ca3`), `shepherd2` 0.1.0a0

This document compares the Python Shepherd monorepo (`../shepherd`) with this Go
port and defines a scoped roadmap for bringing them closer to parity. Detailed
workstream plans live in `plans/`:

| Plan | Workstream | Priority |
|---|---|---|
| [plans/00-execution-plan.md](plans/00-execution-plan.md) | **Phased execution master** — task-level breakdown (T0.1–T4.7), milestones/version plan, dependency graph, risks, verification strategy | — |
| [plans/01-abi-conformance.md](plans/01-abi-conformance.md) | Canonical-JSON byte identity, golden vectors, conformance suite, ABI laws | **P0** |
| [plans/02-schemas-runtime.md](plans/02-schemas-runtime.md) | shepherd2 schema rings (execution, relations, history) + runtime handles | P1 |
| [plans/03-substrate-materialization.md](plans/03-substrate-materialization.md) | vNext substrate registry + materialization dispatch, bridged to `Sandbox` | P1 |
| [plans/04-supervision-settlement.md](plans/04-supervision-settlement.md) | Check-at-commit merge gate, consume-once settlement, fork-and-choose recipes | P2 |
| [plans/05-persistence-hygiene-release.md](plans/05-persistence-hygiene-release.md) | Release/tagging, persistence & recovery, bug batch, CI | **P0 (process)** |

---

## 1. What is being compared

The Python repo is not one kernel — it is three layers, and only the innermost
is the Go port's ABI target:

```
┌────────────────────────────────────────────────────────────────────┐
│ v1 framework: shepherd packages (core/runtime/dialect/contexts/    │  NOT the Go
│ providers/sandboxes/export/transform), sp CLI, permission grants,  │  port's ABI
│ OS jails (Seatbelt/Landlock), ContainerDevice + overlay extraction │  target
├────────────────────────────────────────────────────────────────────┤
│ vcs-core / commons-vcs: provenance-native VCS (world values,       │  NOT the Go
│ refs/vcscore/*, carriers: clonefile/overlayfs/copy), retained-     │  port's ABI
│ output settlement, sessions/daemon                                 │  target
├────────────────────────────────────────────────────────────────────┤
│ shepherd2: the frozen ABI kernel (shepherd.kernel.abi.v0)          │  ← THE Go
│  Ring 0 kernel: canonical v2, records, witnesses, cuts/slices,     │    port
│    SQLiteTraceStore                                                │    mirrors
│  Ring 1 schemas: execution, relations, history, run_outputs        │  ← P1 parity
│  Ring 2 runtime: Run/ChildHandle/TaskControl/@task                 │  ← P1 parity
│  vNext (unfrozen): substrates, materialization dispatch, skeleton  │  ← P1/P2 parity
└────────────────────────────────────────────────────────────────────┘
```

The Go port today = shepherd2 Ring 0 (faithful) + a Go-native supervision and
sandbox layer (effect bus, scope lifecycle, checkpoints, `Sandbox` with git and
containerd backends) that has **no direct shepherd2 counterpart** — it fuses
concerns from the v1 runtime (`ScopeProxy`, `CheckpointManager`) and vcs-core
(carriers/containment) into a smaller, workspace-snapshot-based design. Those
Go-native decisions were made deliberately and are documented in
`GAP-REPORT.md`; this plan does not relitigate them.

**"Parity within reason" therefore means:**

1. Be *byte-identical and conformance-proven* on the frozen ABI (Ring 0).
2. Port the ABI-adjacent layers that make traces *interpretable* (Ring 1
   schemas, Ring 2 handles) and *actionable* (vNext materialization).
3. Adopt the v1/vcs-core **interaction patterns** that sit naturally on the Go
   `Sandbox`/`Scope` primitives (check-at-commit supervision, consume-once
   settlement) without porting their machinery (world values, carriers, jails).
4. Leave the rest out of scope, explicitly (see §4).

---

## 2. Parity matrix

Legend: ✅ at parity · ⚠️ partial or at-risk · ❌ missing in Go · 🔀 deliberate
divergence (documented decision) · ➖ out of scope · 🟦 Go-only strength.

### 2.1 Ring 0 — frozen ABI kernel (`shepherd.kernel.abi.v0`)

| Capability | Python (shepherd2) | Go | Verdict |
|---|---|---|---|
| Canonical JSON v2 + digest | `kernel/canonical.py` (`ensure_ascii=False`, `allow_nan=False`, repr floats) | `canonical.go` (uses `json.Marshal`: HTML-escapes `<>&`; `%d` for integral floats) | ⚠️ byte-identity risk on floats/escapes — **plan 01** |
| Golden vectors (digest layer) | `tests/golden/kernel_abi_v0.json` | `testdata/kernel_abi_v0.json` — **byte-identical file** (SHA-256 verified) | ✅ file; ⚠️ vectors cover ints/strings only — extend — **plan 01** |
| Store-level cross-language vectors | `SQLiteTraceStore` produces witness/context IDs deterministically | no equivalent fixture | ❌ — **plan 01** |
| Append / intents / receipts | `trace_store.py` (1520 L): atomic batches, intent idempotency + conflict, `AppendIntentConflictError` | `store.go` (1912 L): same semantics, WAL, single connection | ✅ |
| Witness chain to root | root sentinel `""`, ordinary `kernel.witness.v1`, support closure, cycle rejection | same, `validateWitnessChain`, synthetic owner `kernel:witness` | ✅ |
| Cuts / frontiers | publish/resolve, immutability, `read_owner_cutoff` re-verification | same (`shepherd2.frontier.owner_cutoff.v1` record + index cross-check) | ✅ |
| Slices: traversal, mode filter, anchors, visibility | `_trace_slice` pipeline, 11 slice laws | equivalent pipeline; `ExternalAnchor.AnchorKind` never populated (arg-position bug) | ⚠️ bug — **plan 01** |
| `TraceStore` protocol surface | `append, preview_*, read_fact, read_owner_prefix, read_path_prefix, publish_cut/frontier, resolve_cut/frontier, read_owner_cutoff, read_causal_closure, close` | all except **`ReadPathPrefix`** (table `path_entries` exists) | ⚠️ — **plan 01** |
| Backend-agnostic conformance suite | `test_trace_store_conformance.py` (438 L, parametrized — explicitly intended to gate a Go backend) | ad-hoc store tests (18) | ❌ port suite — **plan 01** |
| 25 executable ABI laws | `test_kernel_abi_v0_laws.py` (~790 L) | partial coverage across `canonical_test.go`/`store_test.go` | ⚠️ map + close — **plan 01** |
| Authorization model | Slice-A trusted contexts; `full_internal` requires trusted authority; operation kinds | same for append/read; `OpMaterialize`/`OpObserve` declared but unenforced | ⚠️ enforce with materialization — **plan 03** |
| `context.Context` / cancellation | n/a (threading) | store APIs take no `context.Context` | ⚠️ Go-idiom gap — **plan 05** |

### 2.2 Rings 1–2 — schemas and runtime handles

| Capability | Python | Go | Verdict |
|---|---|---|---|
| Execution lifecycle schema (`shepherd2.execution.{created,started,completed,failed}.v1`), deterministic `exec:<32hex>` IDs, batch builders, terminal-frontier law, `project_execution` fold | `schemas/execution.py` (315 L) | ❌ | **plan 02** |
| Relations (`spawned/adopted/abandoned`) + projection | `schemas/relations.py` | ❌ | **plan 02** |
| Effective-history projection | `schemas/history.py` | ❌ | **plan 02** |
| Schema library / projection purity (`ProjectionSpec`, mode requirements) | `schemas/schema_library.py` | ❌ | **plan 02** |
| Run-output descriptor vocabulary (`shepherd2.run_output.v1`, skeleton descriptor/locator) | `schemas/run_outputs.py` (~580 L) | ❌ | ⚠️ minimal subset only — **plan 04** (settlement), rest ➖ |
| Runtime handles: `Run`, `ChildHandle` (wait/snapshot/cutoff), `TaskControl` (publish/spawn/adopt/abandon), `@task` | `runtime/handles.py` (437 L, sync facade, no scheduler) | ❌ | **plan 02** |

### 2.3 vNext — substrates and materialization

| Capability | Python | Go | Verdict |
|---|---|---|---|
| `Substrate` protocol + registry (`substrate_ref`, declaration/capture schemas, containment, `materialize`) | `vnext/substrates.py`, `EchoSubstrate`, `SQLiteKVSubstrate` (`kv.sqlite.local.v1`) | ❌ (Go `Sandbox` is workspace-level, not trace-record-level) | **plan 03** — bridge both concepts |
| `Materialize` dispatch: owner-path-explicit requests, witness-stamped operation, declaration→capture append, completed-intent idempotency ledger | `vnext/materialization.py` (~296 L) | ❌ | **plan 03** |
| Skeleton `Session` over vcs-core (retained outputs, custody validation, seal handoff) | `vnext/skeleton.py` (1302 L) | ➖ requires vcs-core | out of scope; plan 04 takes the settlement *pattern* only |

### 2.4 Scope, checkpoints, materialization mechanics (v1 ↔ Go sandbox layer)

| Capability | Python (v1/vcs-core) | Go | Verdict |
|---|---|---|---|
| Scope fork/merge/discard | `ScopeProxy.child/merge/discard`; vcs-core tree primitives | `Scope`/`ScopeManager` (same verbs, trace-recorded, sandbox-aware) | 🔀 at-parity-in-spirit; Go design kept |
| Fold invariant `state(t)=fold(effects)`, `ImmutableScope`, `ContextBinding`, `Stream` (truncate_to, by_depth, direct) | `shepherd_core.scope.{model,stream}` (215 + 1172 L) | 🔀 **rejected by design** (GAP-REPORT §3): snapshots beat stream replay for real worktrees | keep rejected |
| Checkpoint create/restore | `CheckpointManager` (stream truncate + replay + materialization watermark → `ContainmentError`) | `Checkpoint` + `WorkspaceState` via `Sandbox.Capture/Apply` (snapshot-based, single-use) | 🔀 functionally at parity; mechanism differs by design |
| Checkpoint/scope **durability** | trace records durable; vcs-core projects scope registry | checkpoints + scope registry **in-memory only** — lost on restart | ⚠️ — **plan 05** (recover from trace) |
| File deltas (`FileDelta`/`FileChangeset`, diff-match-patch, zlib+b64, drift hashes) | `shepherd_contexts.simple_workspace.delta` | 🔀 **rejected**: git is the materializer (GAP-REPORT §5) | keep rejected |
| Materialization ordering by reversibility tier (AUTO→COMPENSABLE→NONE, reverse rollback) | `_scope/_materialization.py` | 🔀 rejected (git handles FS uniformly) | keep rejected; see §5 backlog |
| Overlay/container fork-revert (~134 ms, paper) | ContainerDevice + `OverlayEffectExtractor` (Podman, OverlayFS upper-layer walk, whiteouts); vcs-core carriers: clonefile / kernel-overlayfs / fuse / copy | `sandbox/containerd` (overlay **snapshotter** lifecycle: stop→commit→prepare→restart; implemented vs. fakes, daemon path unverified); git worktree carrier | 🔀 different mechanism, same goal; Go path viable — daemon verification **plan 05** |
| OS-level permission enforcement (Seatbelt macOS, Landlock Linux, signature grants `May[GitRepo, ReadOnly\|ReadWrite]`, placements jail/advisory/auto) | dialect + vcs-core containment backends | ❌ (containerd namespace isolation is the only enforcement) | ➖ out of scope; §5 backlog |

### 2.5 Supervision and settlement

| Capability | Python | Go | Verdict |
|---|---|---|---|
| Runtime supervision (observe + inject/halt) | provider-boundary recorder, handler frames | `Supervisor` + `EffectBus` (async rules) — 🟦 Go-only real-time bus | 🟦 ahead |
| Pre-execution interception | ❌ (Python supervises at commit, not pre-tool) | `Supervisor.CheckCall` + `DestructiveToolGuard` (deny) | 🟦 ahead |
| Check-at-commit supervision (`SubstrateOperationProposed` before settlement; approve-by-return / deny-by-raise; decision recorded) | `dialect/supervision.py` (`SupervisorDenied`, `supervisor_frame`) | ❌ — merge is ungated | **plan 04** |
| Retained outputs + consume-once settlement (`select/apply/release/discard`) | vcs-core settlement machine + `RunOutput` verbs | ❌ — merge/discard exist but no seal/review/settle lifecycle | **plan 04** (lightweight, sandbox-level) |
| Fork-and-choose (best-of-n), retry-until-acceptable, apply-onto-moved recipes | `examples/workspace-handles/*` | primitives exist (isolated forks, diffs, checkpoints); no recipes/tests | **plan 04** |

### 2.6 Peripheral (all out of scope)

Providers (claude/openai/litellm/opencode), `sp`/`vcs-core` CLIs, docs site,
export/transform/authoring, extras (banking/coding/citation-checker/trace-viewer),
kernel-v3-reference, commons-vcs convergence, remote sandboxes (e2b/modal/
daytona/kubernetes), packaging wheel. Go port stays a **pure library** (its own
design principle); yaah is the host application.

---

## 3. Roadmap and sequencing

```
Phase 0 — Release hygiene (plan 05 §1–3,5)            ~0.5 wk
  merge feat/containerd → main, tag v0.4.0, LICENSE,
  containerd //go:build linux + CI compile, bug batch
  (AnchorKind, HighErrorRateRule denominator, monotonic
  scope intent IDs, ExecResult.Duration), replace-directive fix

Phase 1 — ABI trust (plan 01)                          ~1–2 wk
  canonical escaper/float parity, extended + store-level
  golden vectors (co-generated with Python), conformance
  suite port, 25-law coverage map, ReadPathPrefix

Phase 2 — Interpret & act on traces (plans 02 + 03, parallelizable)
  02: execution/relations/history schemas, projections,   ~2–3 wk
      schema library, runtime handles
  03: Substrate + registry, Materialize dispatch with     ~1–2 wk
      idempotency ledger, WorkspaceSubstrate bridge over
      Sandbox, EchoSubstrate

Phase 3 — Supervised settlement (plan 04)              ~2–3 wk
  ProposeMerge/CommitMerge gate + supervisor.decision
  records, Seal + RetainedOutput consume-once verbs over
  worktree sandboxes, best-of-n / retry recipes as tests

Ongoing — Durability & idioms (plan 05 §4,6–8)         ~1–2 wk
  scope/checkpoint recovery from trace, snapshot persistence,
  context.Context in store APIs, README/CI polish
```

Phases 0 and 1 first: Phase 0 makes v0.4.0 real and consumable; Phase 1 turns
"byte-identical digests" from a claim into a *tested* property before more
surface is built on top. Phases 2–3 are additive and independently shippable.

**Total estimated effort**: ~7–11 focused weeks for everything above; each
plan is separately mergeable and the phases can stop at any boundary with the
port still coherent.

---

## 4. Explicitly out of scope (with reasons)

| Item | Reason |
|---|---|
| `ImmutableScope` fold model, `Stream.truncate_to`, effect replay | Deliberate GAP-REPORT decision; a real worktree is not derivable from an effect log. Snapshot-based rollback already shipped. |
| `FileDelta`/`FileChangeset` materializer layers | git is the materializer; deltas duplicate it and still miss bash side effects. |
| Reversibility tiers as a typed effect hierarchy | Only pays off with the fold model; Go effects stay schema-ref-matched bus events. (Revisit trigger in §5.) |
| Containment graduation ladder (SANDBOX→SCOPE→MATERIALIZED→ESCAPED) | Go encodes containment per-witness/per-sandbox-capability instead; the ladder is v1-runtime plumbing. |
| vcs-core (world values, `refs/vcscore/*`, carriers, sessions/daemon, fsck) | A second product; the Go `Sandbox` interface + worktree/containerd backends cover the fork/revert use case at a fraction of the surface. |
| OS jails (Seatbelt/Landlock) and signature grants | Enforcement belongs where processes are spawned; containerd backend provides the Linux enforcement story. Backlog item below. |
| Providers, CLI, docs site, extras, packaging | Go port is a library; yaah hosts the loops and tooling. |
| skeleton `Session` custody/seal-handoff machinery | Depends on vcs-core; plan 04 keeps only the settlement state machine. |

## 5. Backlog (revisit triggers, not commitments)

- **Landlock exec-wrapper** for git-sandbox Exec (Linux): a `JailedExec` option
  on `SandboxSpec` would bring OS enforcement to the portable backend. Trigger:
  a consumer runs untrusted code without containerd.
- **Reversibility metadata on schema refs** (cheap subset of tiers: a registry
  mapping schema → AUTO/COMPENSABLE/NONE used to order plan-04 settlement and
  flag irreversible captures). Trigger: plan 03 substrates expose >1 backend
  class with compensation needs.
- **Trace inspection CLI** (`inspect`/`log`/`diff` over a trace file, mirroring
  `vcs-core` CLI verbs). Trigger: debugging a field deployment without yaah.
- **ATIF/trajectory export** of Go traces. Trigger: external eval tooling needs it.
- **macOS clonefile carrier** as a third `Sandbox` backend (fastest fork on
  darwin, mirrors `_clonefile_carrier`). Trigger: containerd-onlyLinux proves
  limiting for yaah's primary platform work.

## 6. Sources

- Python inventory: full-repo exploration of `../shepherd` (shepherd2 rings,
  v1 packages, vcs-core, commons-vcs, containers, docs, examples,
  integration-tests; versions from workspace pyproject + CHANGELOG).
- Go inventory: exploration of this repo at `48b16fb` (all sources, tests,
  README/DEV-PLAN/GAP-REPORT, both go.mod files, tags/reflog).
- Historical decisions: `GAP-REPORT.md` (v0.4.0 resolution table),
  `DEV-PLAN.md` (superseded, principles retained).
- ABI spec: `../shepherd/shepherd2/docs/kernel-abi-v0.md`, `slice-semantics.md`.
