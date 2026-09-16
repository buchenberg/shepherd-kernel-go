# 00 — Phased Execution Plan (Master)

**Date**: 2026-09-16 · **Owner**: buchenberg · **Status**: proposed
**Scope**: shepherd-kernel-go (`github.com/buchenberg/shepherd-kernel-go`, main @ `a07ead4` = v0.3.2; `feat/containerd` @ `48b16fb` = untagged v0.4.0-content)
**Reference**: Python `shepherd` monorepo (`shepherd-workspace` 0.3.1, `shepherd2` 0.1.0a0, main @ `d34d5ca3`)

This is the **execution** document: task-level breakdown, sequencing,
dependencies, milestones, risks, and per-phase entry/exit gates. It ties
together:

- `PARITY-PLAN.md` — the comparison, scope decisions, and what stays out
- `plans/01…05` — per-workstream technical detail (APIs, tests, acceptance)

Where this document and a workstream plan disagree on sequencing, this
document wins; where they disagree on API shape, the workstream plan wins.

---

## 1. Reading guide

- Work is organized into **phases 0–4**. Each phase is independently
  mergeable and ends with a **tagged release** — the library is coherent at
  every phase boundary, and stopping anywhere is a valid outcome.
- Tasks are numbered `T<phase>.<n>`. Each has: action, files touched, test
  obligation, dependencies within/between phases, and an estimate in dev-days
  (1 dev-day ≈ one focused day; estimates include tests but not review lag).
- **Cross-language vectors are the arbiter of parity.** Where a Python
  behavior and Go behavior disagree, the Python-generated vector decides,
  unless PARITY-PLAN §4 records a deliberate divergence.
- "yaah coordination" flags tasks that change APIs yaah consumes
  (`ScopeManager`, `Supervisor`, store). yaah changes themselves are out of
  scope for this repo but must be scheduled against the phase exit.

## 2. Guiding principles

1. **Trust before surface.** Do not build new schema layers (phase 2) on a
   canonicalizer with byte-identity risk (fixed in phase 1).
2. **Every phase ends consumable.** Tags exist, `go get` resolves, no
   `replace` directives in tagged modules, CI green.
3. **Vectors over prose.** Every parity claim ships with a Python-generated
   fixture checked in and hash-pinned in CI.
4. **Deliberate divergences are load-bearing.** The fold-invariant scope
   model, file-delta materialization, containment ladder, and vcs-core world
   values stay unported (PARITY-PLAN §4, GAP-REPORT resolution table). Tasks
   that would reopen them require an explicit decision note first.
5. **Library purity.** No host-app imports (yaah), no LLM provider knowledge,
   no CLI in the core module. Recipes ship as tests, not binaries.

## 3. Milestones and version plan

| Milestone | Tag | Phase | Contents | Target (weeks from start) |
|---|---|---|---|---|
| M0 | **v0.4.0** (+ `sandbox/containerd/v0.1.0`) | 0 | Sandbox substrate shipped, tagged, consumable; bug batch; CI bootstrap | W1 |
| M1 | **v0.5.0** | 1 | Canonical byte-identity proven; conformance suite; store vectors; `ReadPathPrefix` | W2–3 |
| M2 | **v0.6.0** | 2a | Execution/relation/history schemas, projections, runtime handles | W4–6 |
| M3 | **v0.7.0** | 2b | Substrates, materialize dispatch + ledger, `WorkspaceSubstrate` | W5–7 (overlaps 2a) |
| M4 | **v0.8.0** | 3 | Merge review gate, retained-output settlement, recipes | W7–9 |
| M5 | **v0.9.0** | 4 | Trace-based recovery, checkpoint durability, `context.Context` sweep | W9–11 |
| — | v1.0 decision | gate | §9 readiness checklist review | W11+ |

Pre-1.0 semver policy: breaking API changes are allowed in minor bumps but
must land only at phase exits (M1/M5 carry the planned breaks: digest
behavior change in v0.5.0, `context.Context` signatures in v0.9.0).

## 4. Dependency graph

```
T0.* (hygiene, bugs, tags, CI bootstrap)
  └─► Phase 1 (ABI trust) ─────────────┐
        T1.1/T1.2 canonical ─► T1.3 vectors ─► T1.5 store vectors
        T1.6 ReadPathPrefix (indep)      │
        T1.7 conformance ◄─ T1.6         │
        T1.8 laws ◄─ T1.7                │
                                         ▼
                     ┌────────── Phase 2a (schemas/runtime) ──┐
                     │  T2a.1 execution ─► T2a.2 relations ─► T2a.3 history
                     │  T2a.4 schema-lib (needs T2a.1)        │
                     │  T2a.5 handles (needs T2a.1–4)         │
                     └─────────────────────────────────────── ▼ tag v0.6.0
                     ┌────────── Phase 2b (substrates) ───────┐   (parallel w/ 2a)
                     │  T2b.1 interface ─► T2b.2 dispatch+ledger
                     │  T2b.3 echo/kv (needs T2b.1)           │
                     │  T2b.4 WorkspaceSubstrate (needs T2b.2)|
                     └─────────────────────────────────────── ▼ tag v0.7.0 (after v0.6.0 merges)
                                         │
                     Phase 3 (gate+settlement) ◄── soft dep on 2a (run identities)
                        T3.1–T3.4 merge gate ─► T3.5–T3.6 settlement ─► T3.7 recipes
                                         ▼ tag v0.8.0
                     Phase 4 (durability+idioms)
                        T4.1 recovery (after T3.x if outputs exist) ─► T4.2 snapshots
                        T4.5 ctx sweep (last breaking change)
                                         ▼ tag v0.9.0
```

Hard gates: **Phase 1 → Phase 2** (digest stability), **v0.4.0 tag → nested
containerd tag** (replace-directive removal needs a resolvable core version).
Everything else is parallelizable by task.

---

## 5. Phase 0 — Release hygiene & bug batch (W1, ~3–4 dev-days)

**Goal**: make the shipped work real: `v0.4.0` exists, modules resolve, known
bugs are fixed in one batch, minimal CI runs.
**Entry criteria**: `feat/containerd` passes `go test ./...` on Windows + a
Linux spot-check (WSL/CI).
**Deliverable**: tags `v0.4.0` and `sandbox/containerd/v0.1.0`; `CHANGELOG.md`; LICENSE.

| ID | Task | Files | Est | Deps |
|---|---|---|---|---|
| T0.1 | Review + merge `feat/containerd` → `main` via PR | `sandbox.go`, `sandbox_git.go`, `workspace.go`, `checkpoint.go`, `sandbox/containerd/` | 0.5d | — |
| T0.2 | Reconstruct `CHANGELOG.md` (v0.1.0→v0.4.0 from tags/reflog); tag v0.4.0 | `CHANGELOG.md` | 0.5d | T0.1 |
| T0.3 | LICENSE decision + file (**owner gate**: align with upstream `../shepherd/LICENSE`; do not guess the family) | `LICENSE` | 0.25d | owner input |
| T0.4 | containerd module publishability: remove `replace`, bump `require` to tagged core, add `//go:build linux` split (portable `sandbox.go` vs tagged `client.go`/`backend_linux.go`), tag `sandbox/containerd/v0.1.0` | `sandbox/containerd/go.mod`, `client.go`, new `backend_*.go` | 1d | T0.2 |
| T0.5 | **Manual live-daemon smoke** (WSL2 + containerd): create → WriteFile → Capture → mutate → Apply → Exec → Destroy. Record result in README status note; file bugs found as T0.7+ or phase-2b blockers | (no repo changes; findings) | 0.5d | T0.4 |
| T0.6 | Bug batch (single PR, plan 05 §5): ① `HighErrorRateRule` denominator (`supervisor.go` ~393); ② `ExternalAnchor.AnchorKind` arg-position (`store.go` ~1034/1098); ③ scope-event intent IDs → atomic seq (`scope.go`); ④ `ExecResult.Duration` populated in containerd `Exec`; ⑤ stale `TreeState`/`DiffSince` comments (`sandbox.go:44`, `workspace_test.go:282`); ⑥ regression test: `Capture` with caller's pre-staged index | `supervisor.go`, `store.go`, `scope.go`, `sandbox/containerd/sandbox.go`, misc | 1d | T0.1 |
| T0.7 | CI bootstrap (`ci.yml`): matrix `ubuntu/windows/macos` × `go test ./... -count=1 -race` (core); linux-only job builds+tests nested module; `go vet`; `gofmt -l` fail-on-output | `.github/workflows/ci.yml` | 0.5d | T0.1 |

**Exit criteria / acceptance**:
- [ ] `go get github.com/buchenberg/shepherd-kernel-go@v0.4.0` resolves and
      builds; nested module resolvable without `replace`.
- [ ] CI green on 3 OSes; nested module compiles on linux, is excluded (via
      build tags) — not broken — elsewhere.
- [ ] All six bug-batch fixes merged with tests; T0.5 smoke result recorded
      (pass → phase 2b de-risked; fail → containerd findings converted to
      tasks before T2b.7).
- [ ] CHANGELOG + LICENSE present.

**yaah coordination**: v0.4.0 is API-compatible with what yaah's
`feat/containerd`-era code already expects; publish the tag so yaah can pin it.

---

## 6. Phase 1 — ABI trust (W2–3, ~8–10 dev-days) → v0.5.0

**Goal**: turn "byte-identical digests" into a tested property across the
whole store path. Detailed spec: `plans/01-abi-conformance.md`.
**Entry criteria**: M0 tagged; Python repo available locally at `../shepherd`
(pinned tag recorded for vector generation).
**Deliverable**: v0.5.0 — **includes a digest-behavior change** (payloads with
`< > &` or non-integer floats hash differently, correctly, after this phase;
CHANGELOG must call this out; old traces remain readable — only new digests
change).

| ID | Task | Files | Est | Deps |
|---|---|---|---|---|
| T1.1 | Canonical string escaper: replace `json.Marshal` string path with Python-matching escaping (raw UTF-8, no HTML escaping, `\u00xx` lowercase control chars); reject lone surrogates | `canonical.go` | 1d | — |
| T1.2 | `formatCanonicalFloat`: CPython `float_repr` semantics — shortest round-trip digits, fixed for `1e-4 ≤ |x| < 1e16`, else scientific (`1e+16`, `1e-05`), `-0.0` preserved, NaN/±Inf → explicit error; `json.Number` passthrough validated | `canonical.go` | 1.5d | — |
| T1.3 | Property test corpus: generate ~200 mixed payloads (floats/strings/nesting) via Python `canonical_digest`; Go tests reproduce every digest | `canonical_test.go`, `testdata/canonical_corpus_v0.json` | 1d | T1.1, T1.2 |
| T1.4 | Extend shared golden file: edge vectors (floats `0.0/-0.0/1e16/1e-7`, escapes `<>&`, control chars, unicode) co-generated with Python (`generate_edge_vectors.py` contributed upstream **or** vendored with `generated-by: shepherd2@<sha>` provenance); CI golden-hash-drift job (clone pinned shepherd tag, compare hashes) | `testdata/kernel_abi_v0.json`, `.github/workflows`, (upstream script) | 1d | T1.2 |
| T1.5 | Store-level vectors: fixture batches (multi-group, retained contexts incl. reuse-by-ID, local refs, frontier publish, intent replay) with Python-produced record/context/frontier IDs → `testdata/store_vectors_v0.json`; Go replay test asserts ID-for-ID equality | `store_test.go` or new `store_vectors_test.go`, `testdata/` | 1.5d | T1.1–T1.4 |
| T1.6 | `ReadPathPrefix(ctx, path, through, modeFilter) (Slice, error)` per `TraceStore` protocol (`path_entries` table already exists); ctx-taking `ReadOwnerCutoff` variant | `store.go`, `types.go` | 1d | — (parallel) |
| T1.7 | Conformance suite port: `runConformance(t, factory)` translating every case in `test_trace_store_conformance.py`; PR includes checkbox mapping Python-test → Go-test / "N/A + reason" | new `conformance_test.go` | 1.5d | T1.6 |
| T1.8 | 25-law coverage map (`docs/law-coverage.md`): each law in `test_kernel_abi_v0_laws.py` → covering Go test; add missing (expected: duplicate-parent rejection, local-ref pre-retention resolution, witness-cycle rejection, support-closure dedupe/mode-independence, closure-policy anchor behavior, mode-filter non-pruning, non-cross-authorization for cut publication) | `canonical_test.go`, `store_test.go`, new `laws_test.go` | 1.5d | T1.7 |
| T1.9 | `OpMaterialize`/`OpObserve`: document as reserved (enforcement lands in T2b.2); add behavior-pinning test | `types.go`, `store.go` doc comments | 0.25d | — |

**Exit criteria / acceptance** (mirrors plan 01 §8):
- [ ] Corpus + extended goldens + store vectors all pass; CI fails on golden drift.
- [ ] Conformance suite complete or N/A-documented; law map has no ❌ without rationale.
- [ ] `-race` green; v0.5.0 tagged; CHANGELOG flags the digest-behavior fix.

**yaah coordination**: after v0.5.0, traces written by yaah on ≥v0.5.0 are
cross-readable with Python; traces written by older Go versions with
edge-case payloads are **not** digest-compatible — note in yaah release notes.

---

## 7. Phase 2a — Schema rings & runtime handles (W4–6, ~10–12 dev-days) → v0.6.0

**Goal**: make Go traces interpretable — executions, relations, history, with
a sync task facade. Detailed spec: `plans/02-schemas-runtime.md`.
**Entry criteria**: v0.5.0 (canonical parity is a hard prerequisite — schema
IDs are digests).
**Deliverable**: v0.6.0 with new root-package files and no new dependencies.

| ID | Task | Files | Est | Deps |
|---|---|---|---|---|
| T2a.1 | Execution schema: refs `shepherd2.execution.{created,started,completed,failed}.v1`, `ExecutionIDFor` (verify exact Python derivation before coding), draft builders, `Create/Complete/FailExecutionBatch`, `PublishExecutionFrontier` (+ terminal-frontier law), `ProjectExecution(FromStore)` fold | `execution.go` (+test) | 2d | phase 1 |
| T2a.2 | Relations: `shepherd2.execution_relation.created.v1`, spawned/adopted/abandoned, parent-owner-path invariant tests | `relations.go` (+test) | 1d | T2a.1 |
| T2a.3 | History: `PublishedFact`/`EffectiveChild`/`EffectiveHistory` + projections from slice/store | `history.go` (+test) | 1d | T2a.1–2 |
| T2a.4 | Schema library: `ProjectionSpec`, `StaticSchemaLibrary`, `EnsureProjectionCompatible`, default `ShepherdSchemas()` registry; closes law "projections fail on incompatible mode filter" | `schema_library.go` (+test) | 1d | T2a.1 |
| T2a.5 | Runtime handles: `TaskFunc`, `Registry`, `StartTask`/`StartTaskSync`, `Run`, `ChildHandle` (Wait/Snapshot/Cutoff), `TaskControl` (CausalTail/Publish/Spawn/Adopt/Abandon/AwaitTerminal/ReadExecution); owner-path convention mirrored from Python | `handles.go` (+test) | 3d | T2a.1–4 |
| T2a.6 | Cross-language vectors: `execution_id_for`/relation-ID derivations, full create→start→complete batch sequence IDs, `StartTaskSync` trace ID-identity vs Python `@task` fixture | `testdata/`, vector tests | 1.5d | T2a.5, T1.4 process |
| T2a.7 | Integration: task tree with spawn+adopt+abandon+publish; effective-history projection from frontier; pending→running→succeeded transitions; fail path; **restart law** (reopen store file, re-project identical) | `handles_integration_test.go` | 1.5d | T2a.5 |
| T2a.8 | README "Executions & handles" section | `README.md` | 0.25d | T2a.7 |

**Exit criteria**: all plan-02 §7 boxes checked; v0.6.0 tagged.
**Note**: purely additive — no breaking changes; yaah can adopt lazily.

---

## 8. Phase 2b — Substrates & materialization (W5–7, ~6–8 dev-days) → v0.7.0

**Goal**: typed declaration→capture substrate dispatch with an idempotency
ledger, bridged to `Sandbox`. Detailed spec: `plans/03-substrate-materialization.md`.
**Entry criteria**: v0.5.0. Runs in parallel with 2a; **tag order**: v0.7.0
cut only after v0.6.0 merges (avoid interleaved minors).
**Deliverable**: v0.7.0.

| ID | Task | Files | Est | Deps |
|---|---|---|---|---|
| T2b.1 | `Substrate` interface, `SubstrateRegistry`, `MaterializationResult`/`Receipt`, outcome vocabulary, `ErrUnknownSubstrate` | `substrate.go` (+test) | 1d | phase 1 |
| T2b.2 | `Materialize` dispatch: owner-path-explicit `MaterializationRequest`, target validation (decl schemas, ordinal ≤ cutoff), request digest, `materialization_ledger` table, replay/conflict semantics, witness-stamped capture append; ordering (append-before-ledger) verified against Python; enforce `OpMaterialize` | `materialize.go` (+test) | 2.5d | T2b.1 |
| T2b.3 | `EchoSubstrate` + SQLite KV substrate (`kv.sqlite.local.v1`, `shepherd2.kv.put.v1` → `.applied.v1`, separate world-side file per Python's separation) | `substrate_echo.go`, `substrate_kv.go` (+tests) | 1.5d | T2b.1 |
| T2b.4 | `WorkspaceSubstrate` over `Sandbox`: declaration schemas `workspace.file.{write,delete}.v1`, `workspace.exec.v1`; capability-gated dispatch (git in-place → honest failure; worktree/containerd → full); applied-captures with path digest / exit code / stdout digest | `substrate_workspace.go` (+test, fake sandbox) | 2d | T2b.2 |
| T2b.5 | Cross-language vectors: Echo + `materialize` capture record IDs vs Python for identical requests | `testdata/` | 0.75d | T2b.3 |
| T2b.6 | README "Materializing recorded intents" section | `README.md` | 0.25d | T2b.4 |
| T2b.7 | containerd live integration (linux CI, env-gated `SHEPHERD_CONTAINERD_ADDR`): WorkspaceSubstrate over live daemon — write→capture→destroy→apply→verify; consumes T0.5 findings | `sandbox/containerd/live_test.go`, CI | 1d | T2b.4, T0.5 |

**Exit criteria**: plan-03 §6 boxes checked; ledger idempotency proven by
crash-window test; v0.7.0 tagged.

---

## 9. Phase 3 — Supervision gate & settlement (W7–9, ~10–12 dev-days) → v0.8.0

**Goal**: check-at-commit review + consume-once output settlement on the
existing `Scope`/`Sandbox` primitives, with the three reference recipes as
tests. Detailed spec: `plans/04-supervision-settlement.md`.
**Entry criteria**: v0.6.0 preferred (executions give runs identities) but
not hard-required — settlement addresses scopes/outputs, not executions.
**Deliverable**: v0.8.0.

| ID | Task | Files | Est | Deps |
|---|---|---|---|---|
| T3.1 | Fork-time baseline: `ForkIsolated`/`Fork` capture parent `WorkspaceState` digest into `shepherd.scope.forked.v1` payload (`baseline_digest`, additive) | `scope.go`, `scope_manager.go` (+tests) | 0.75d | — |
| T3.2 | `ProposedChange`/`MergeProposal` + `ScopeManager.ProposeMerge` (diff vs baseline; shared-sandbox fallback = latest parent capture) | `scope_manager.go` or new `merge_gate.go` | 1.5d | T3.1 |
| T3.3 | `CommitMerge` + reviewer contract + `SupervisorDeniedError`; records `shepherd.merge.proposed.v1` and `shepherd.supervisor.decision.v1`; deny → Discard + error; plain `Merge` remains (documented opt-out) | same | 1.5d | T3.2 |
| T3.4 | Built-in reviewers (`DraftsOnlyReviewer(prefix)`, `DestructivePathReviewer(patterns)`) + `Supervisor.ReviewMerge` (gives `InterventionDiscard` a live emitter) | `supervisor.go` (+tests) | 1d | T3.3 |
| T3.5 | Settlement core: `RetainedOutput`, `Scope.Seal` (+`shepherd.run_output.{sealed,settled}.v1`), state machine (`unconsumed → selected/applied/released/discarded`, `invalid`), `ErrOutputConsumed`, `ErrApplyConflict` (apply failure does **not** consume); select = fast-forward-only; apply = three-way with path-disjointness guard | new `settlement.go` (+tests) | 3d | T3.1 |
| T3.6 | `ScopeManager` output registry (`Seal/Output/OutputsForScope/Settle`) | `scope_manager.go` | 0.75d | T3.5 |
| T3.7 | Recipes as tests: `TestExample_BestOfN`, `TestExample_RetryUntilAcceptable` (CheckCall pre-deny + merge-review commit-deny + checkpoint/restore in one loop), `TestExample_ApplyOntoMovedWorkspace` (disjoint succeeds / overlap conflicts unconsumed) — must pass on Windows git-worktree carrier | `examples_test.go` | 2d | T3.3–T3.6 |
| T3.8 | README "Review before merge / settle outputs" section with best-of-n snippet | `README.md` | 0.5d | T3.7 |

**Exit criteria**: plan-04 §5 boxes checked; recipes green on Windows + Linux;
v0.8.0 tagged.
**yaah coordination**: `supervised_task` review sessions can adopt
`ProposeMerge`/settlement incrementally; additive API.

---

## 10. Phase 4 — Durability & idioms (W9–11, ~6–8 dev-days) → v0.9.0

**Goal**: close the restart-durability gap and land the last breaking idiom
change. Detailed spec: `plans/05-persistence-hygiene-release.md` §4, §6–8.
**Entry criteria**: v0.8.0 (recovery must understand settlement records).
**Deliverable**: v0.9.0 — **breaking**: `context.Context` becomes the first
parameter across store APIs.

| ID | Task | Files | Est | Deps |
|---|---|---|---|---|
| T4.1 | `RecoverScopes(store, opts)`: rebuild scope tree + terminal states from lifecycle records; sandbox re-adoption via `opts.Resolver(scopeID, backend)`; orphan policy | `scope_manager.go`, new `recovery.go` (+tests) | 2d | — |
| T4.2 | Checkpoint durability: `checkpoint_snapshots(checkpoint_id, snapshot BLOB)` table + size guard; recovery marks checkpoints `Invalid` when `state_digest` ≠ fresh `Capture()`; `PruneCheckpoints` deletes blobs | `checkpoint.go`, `store.go` (+tests) | 1.5d | T4.1 |
| T4.3 | Output recovery from sealed/settled records (state_digest+backend+revision sufficient) | `settlement.go`, `recovery.go` | 1d | T4.1, phase 3 |
| T4.4 | `PruneTerminal()` for long-lived hosts (terminal scopes stay lookupable until pruned) | `scope_manager.go` | 0.25d | T4.1 |
| T4.5 | `context.Context` sweep: first-param ctx on all store `Append/Preview/Read/Resolve/Publish/Close`; `database/sql` ctx variants; mechanical yaah call-site updates scheduled alongside | `store.go`, all callers in-repo | 2d | all prior |
| T4.6 | Tag-time verification job: clean-checkout module build, no `replace` in tagged modules (gorelease or equivalent) | CI | 0.5d | T0.7 |
| T4.7 | Docs pass: README claims audit (drop rot-prone exact test counts; document ctx APIs, durability), PARITY-PLAN status table ✅ per landed item | `README.md`, `PARITY-PLAN.md` | 0.5d | T4.5 |

**Exit criteria**: restart round-trip suite green (fork→checkpoint→close→
recover→restore; moved-workspace staleness → invalid; outputs recovered);
v0.9.0 tagged; yaah pinned and compiling against ctx APIs.

---

## 11. Risks & mitigations

| # | Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|---|
| R1 | Float canonicalization subtly diverges from CPython repr on rare values | Medium | **High** (silent digest drift) | T1.3 corpus (200+ Python-generated payloads incl. randomized doubles); exact-threshold implementation, not `%v`; CI drift gate (T1.4) |
| R2 | Python-side vector generation blocked (no upstream PR acceptance) | Medium | Low | Vendor generated files with `generated-by: shepherd2@<sha>` provenance + hash-pinned CI clone (plan 01 §3 fallback) |
| R3 | containerd live daemon path broken (never executed) | Medium | Medium | T0.5 smoke lands in W1 — findings become T2b.7 blockers early; fakes keep lifecycle logic tested everywhere |
| R4 | v0.5.0 digest change breaks yaah trace consumers | Low | Medium | CHANGELOG + release notes at T1 exit; old traces still readable; only new records hash differently |
| R5 | Breaking ctx sweep (T4.5) churns yaah | Low | Low | Sweep isolated to final phase; mechanical diff; yaah PR prepared in parallel |
| R6 | Scope creep toward vcs-core parity (world values, carriers, jails) | Medium | High (schedule) | PARITY-PLAN §4 is the fence; any task touching it requires a written decision note reversing a GAP-REPORT rejection |
| R7 | `execution_id_for` / owner-path conventions guessed wrong → cross-language traces diverge | Medium | Medium | T2a.6 vectors are mandatory before v0.6.0; "verify against Python before coding" written into T2a.1 |
| R8 | Single-maintainer estimate slippage | High | Medium | Every phase independently shippable; milestones are tags, not dates |

## 12. Verification strategy (applies to all phases)

1. **Vectors**: every cross-language claim pinned by a Python-generated
   fixture in `testdata/`; CI hash-compares shared files against a pinned
   upstream tag.
2. **Conformance**: `runConformance(t, factory)` gates any store-like backend
   (T1.7); reused by plan-03 substrates where applicable.
3. **Matrix**: 3-OS `-race` suite from phase 0; git-backed tests self-skip
   without git; containerd live tests env-gated on linux.
4. **Recipes as tests**: user-facing guarantees (best-of-n, retry, apply-on-
   moved) are executable examples, never prose-only (T3.7).
5. **Restart laws**: every durable claim re-tested across store close/reopen
   (T2a.7) and process recovery (T4.1–3).
6. **Status honesty**: PARITY-PLAN matrix rows flip to ✅ only when the
   covering test is named in the PR (T4.7 discipline, applied throughout).

## 13. v1.0 readiness checklist (gate after M5)

- [ ] All plans 01–05 acceptance criteria checked; law-coverage map complete.
- [ ] Golden/store/schema vectors green in CI against current upstream tag;
      upstream shepherd2 version recorded in go-adjacent
      `testdata/UPSTREAM.md`.
- [ ] Live containerd verification recorded; nested module consumable.
- [ ] Durability: full restart-recovery suite green on 3 OSes.
- [ ] `context.Context` throughout; no wall-clock-ordered IDs.
- [ ] LICENSE, CHANGELOG through v0.9.0, README audited.
- [ ] yaah integrated on ≥v0.9.0 for one full release cycle without
      workarounds.
- [ ] Decision: API freeze of Ring-0-adjacent surface (store, canonical,
      schemas, handles) as the v1 contract; Sandbox/Supervisor remain
      explicitly extensible.

## 14. Execution conventions

- One PR per task ID (or per tight cluster ≤1.5d); PR description cites
  `T<phase>.<n>` and the covering workstream plan section.
- Branch naming: `parity/p<phase>-t<n>-<slug>` off `main`.
- No task starts if its dependency's exit criteria are unmet; phase exits are
  tag events, never "when it feels done".
- Estimate drift >2× on any task → stop, split the task, update this document
  in the same PR.
- This document is updated (status column per task: ⬜/🔄/✅) as work lands;
  it is the single source of sequencing truth.
