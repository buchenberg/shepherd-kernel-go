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

## 0. Status — verified against the tree, 2026-10-05

*Added during a reconciliation pass. Every entry below was checked against git
tags, commit contents, and file presence — not inferred from the plan's own
prose. Legend: ✅ done · 🔄 partial · ⬜ not started.*

**HEAD at verification:** `55b4daf` (2026-09-17), **11 commits past `v0.4.0`**, none
of them released for the core module.

**The working tree is now clean** apart from this plan edit and an untracked `.kilo/`
(tooling state). The containerd work that was in flight at the first pass has been
committed as **`603fdcf`** — six files, +1202/−48 — and the live harness is now
tracked. That commit is what T0.5's verdict below is based on.

### ⚠️ Finding first: M0's tag does not contain M0's contents

| Fact | Evidence |
|---|---|
| `v0.4.0` points at | `0c62788` — "Merge pull request #4 from `buchenberg/feat/containerd`" (2026-09-16) |
| Bug batch `4203993` contained in `v0.4.0`? | **No** — `git merge-base --is-ancestor 4203993 v0.4.0` → exit 1 |
| Core tags containing the bug batch | **NONE** |
| Tag that does contain it | `sandbox/containerd/v0.1.0` → `e9252c9` (2026-09-17), i.e. *later* than the core tag |

M0 is defined as *"Sandbox substrate shipped, tagged, consumable; bug batch; CI
bootstrap"* (§3). The tag exists; the bug batch and CI do not. Anyone resolving
`go get …@v0.4.0` gets the pre-fix code, and the nested containerd module pins
exactly that version while its own tree already carries the fixes.

**Decided (§15, 2026-10-05): cut `v0.4.1`** at/after HEAD before any Phase 1
work — tracked as **T0.8** below. It is a pure patch release (bug batch only, no
API break), it makes §3's M0 claim true, and it unblocks `yaah`, which otherwise
has to pin a pseudo-version. Do **not** fold the bug batch into `v0.5.0` — that
release already carries a deliberate digest-behaviour break (R4) and should not
also be a bug-fix vehicle.

### Phase 0 — Release hygiene & bug batch

| ID | Task | Status | Evidence |
|---|---|---|---|
| T0.1 | Merge `feat/containerd` → `main` | ✅ | `0c62788` (PR #4) |
| T0.2 | CHANGELOG + tag `v0.4.0` | 🔄 | CHANGELOG ✅ (`89ebeb3`, reconstructs v0.1.0→v0.4.0); **tag mis-positioned** — see finding above |
| T0.3 | LICENSE | ✅ | `a7562d0` — MIT, matching upstream |
| T0.4 | containerd publishability + tag | 🔄 | `40a2178` removes `replace`, `require`s tagged `v0.4.0`; tag `sandbox/containerd/v0.1.0` exists. **Build-tag split incomplete**: the only `//go:build` in the nested module is on `live_test.go` (`linux && !nolive`); no portable/tagged split of `client.go` as specified |
| T0.5 | Manual live-daemon smoke | ✅ | **Passed 2026-10-07 against containerd v2.3.5 (overlayfs).** The harness (`603fdcf`) found and fixed **nine defects**, then the first soak **failed 3 of 5 runs with a different test failing each time** — a shared-resource race, not per-test bugs. Root cause: the adapter held **no containerd lease**, and this daemon's GC is aggressive (`mutation_threshold = 100`, `schedule_delay = '0s'`, `startup_delay = '100ms'`), so `Capture`'s stop-then-prepare window left its just-committed snapshot unreferenced — observed as `parent snapshot shepherd/<id>/committed/1 does not exist`. Fixed by T0.9. **Re-soak: 12 of 12 green**, zero leaked containers/tasks/snapshots |
| T0.5b | Record the live-daemon result | ✅ | `README.md` states the daemon path is **verified**, naming the ten defects and the 12/12 soak; `CHANGELOG.md` v0.4.1 carries the same |
| T0.6 | Bug batch (6 items) | ✅ | `4203993` touches `checkpoint.go`, `sandbox.go`, `sandbox_git.go`, `scope.go`, `store.go`, `supervisor.go`, `types.go` + both containerd files, with tests |
| T0.7 | CI bootstrap | ✅ | `7281fe1` + `3cf961d` + `55b4daf`; `ci.yml` has the 3-OS matrix, `gofmt` fail-on-output, `go vet`, `go test -race`, `go build -trimpath`, and a linux containerd job |
| T0.8 | **Cut `v0.4.1`** so a core release contains the Phase 0 bug batch | ✅ | Merged as `bee4ca5` (PR #10) and **both tags re-pointed there before publishing** — the first cut at `3315b3a` predated the review fixes. `v0.4.1` resolves from a clean module context and contains the bug batch (`git merge-base --is-ancestor 4203993 v0.4.1` succeeds). Core release is sound |
| T0.8b | **Publish `sandbox/containerd/v0.1.2`** carrying the `v0.4.1` requirement, so the released nested module is not pinned to pre-fix core | ✅ | Tagged at `df47ec9` — the merge commit that carries the repin — and published. Verified end-to-end from a clean module: `go get …/sandbox/containerd@v0.1.2` pulls core `v0.4.1`. `v0.1.1` stays permanently wrong; see the T0.8b note below |
| T0.9 | **Hold a containerd lease for the sandbox's lifetime** so its snapshots cannot be garbage-collected | ✅ | `8e9b867`. Created in `Create`/`Apply` via `ensureLease`, injected into every snapshotter call by `namespaceSnapshotter` (atomic `leaseRef`, so the hot path takes no lock), released in `Destroy`, labelled `shepherd.lease=sandbox` so an orphan from a crashed process is findable. **Accept met: 12/12 consecutive green soak runs** (was 3/5 failing). Ten daemon-free tests in `lease_test.go` pin the bookkeeping; `TestLive_LeaseHoldsSnapshots` proves the lease lists the sandbox's own snapshot keys, and `TestLive_DestroyReleasesTheLease` proves it is dropped |

**Phase 0 is complete.** T0.5 passed, T0.9 proved out the fix, T0.8 published the
core release, and T0.8b published the nested release that carries the repin. The
live suite is deterministic at 12/12, and both module paths resolve to fixed code
from a clean consumer.

**T0.8 was recorded as fully done while only half of it was, and review caught
it — worth keeping as a shape to watch for.** Repinning `sandbox/containerd/go.mod`
(PR #11) and *releasing* the nested module are different acts: the tags were
pushed before PR #11 existed, so `v0.1.1` shipped requiring the pre-fix core, and
it **cannot be corrected in place** because Go module versions are immutable once
the proxy and `sum.golang.org` record them. Hence T0.8b: a new version, not a
retag. The exit criterion is now split into two boxes rather than one.

**The stop-gate did its job, and how it did so is worth recording.** T0.5 was
written as a de-risking step so a fatal containerd problem would surface before
Phase 2b built on it. It surfaced a tenth defect — GC-vulnerable snapshots — that
**no amount of unit testing could have caught**, because the fakes seed their
rootfs key directly and have no garbage collector. And a backend that *sometimes*
loses its workspace is worse than one that fails outright: the symptom would have
looked like anything but a containerd GC, in a long agent run, intermittently.

**Phase 2b is therefore unblocked, and so is `yaah`'s isolated-workspace
activation.** Both were gated on a green soak, not on a judgement call.

**Note on the `v0.4.1` tag move.** The tag was created at `3c5e96c` before T0.9
landed, and its CHANGELOG carried a "Known limitation — containerd backend is not
yet reliable" section. Since the tag had **never been pushed**, folding the fix in
and moving the tag was preferable to publishing a release whose own notes describe
a defect that is fixed in the same unreleased state. If the tag had been pushed,
the correct move would instead have been a `v0.4.2`.

**Where this stands (2026-10-07): both releases are done and verified.** PR #10
merged as `bee4ca5`, and both tags were re-pointed there
before publication — the first cut pointed at `3315b3a`, which predated the review
fixes, so publishing it would have shipped a `v0.4.1` without them. Both core and
nested tags are now **published at fixed content**, verified from a clean module
context:

```
github.com/buchenberg/shepherd-kernel-go                     v0.4.1
github.com/buchenberg/shepherd-kernel-go/sandbox/containerd  v0.1.2
```

**One caveat worth remembering:** the tags are only as good as the check that they
point at merged, reviewed history. Publishing a stale tag is easy to do and
invisible until a consumer compiles against it — so the check is
`git merge-base --is-ancestor <last-fix-commit> <tag>` before every push.

**✅ T0.8b is done — the nested release now carries the repin.** Publishing the tags
and repinning the module were two separate acts, and review caught that only the
first had been completed. The tags were pushed **before** PR #11 merged, so
`sandbox/containerd/v0.1.1` shipped requiring the pre-fix core. That version
**cannot be repaired in place** — Go module versions are immutable once fetched
through the proxy and recorded in `sum.golang.org` — so it took a new version:

| Ref | Requires core | Published? |
|---|---|---|
| `sandbox/containerd/v0.1.0` | `v0.4.0` | yes (historical) |
| `sandbox/containerd/v0.1.1` | `v0.4.0` | yes — **permanently wrong; do not use** |
| `sandbox/containerd/v0.1.2` | **`v0.4.1`** | ✅ **yes — use this one** |

Verified end-to-end from a clean module, not inferred from the tag list:

```
$ go get github.com/buchenberg/shepherd-kernel-go/sandbox/containerd@v0.1.2
go: added github.com/buchenberg/shepherd-kernel-go v0.4.1
$ go list -m all | grep shepherd
github.com/buchenberg/shepherd-kernel-go                    v0.4.1
github.com/buchenberg/shepherd-kernel-go/sandbox/containerd  v0.1.2
```

The core release was never affected: `v0.4.1` is correct and contains the bug
batch. **Consumers must require `v0.1.2`, not `v0.1.1`** — `v0.1.1` still resolves
fine and silently compiles against pre-fix core, which is the failure mode worth
naming. Nothing imports the nested module yet (`yaah`'s sandbox construction is
its own step 5), so this cost nothing in practice; it would have been silent for
the first consumer.

### Phase 1 — ABI trust (v0.5.0)

**Complete — all nine tasks done, verified against the tree 2026-10-07.** The canonical and
store-identity work merged as PRs #12–#14; the conformance suite, law coverage and corpus as PRs #15–#16, both hardened by review (PR #15's review caught a vacuous restart case and a too-narrow receipt assertion; PR #16's caught a corpus advertising subnormals and the 1e16 boundary while containing neither). Rows re-checked against git history
and file presence:

| ID | Task | Status | Evidence |
|---|---|---|---|
| T1.1 | Canonical string escaper | ✅ | `72e3669`: hand-written writer replaces `json.Marshal` — raw UTF-8, no HTML escaping, lowercase `\u00xx` controls, lone surrogates rejected |
| T1.2 | `formatCanonicalFloat` | ✅ | `72e3669`: CPython repr semantics (shortest round-trip, fixed notation inside `1e-4…1e16`, scientific outside, `-0.0`, NaN/±Inf → error); confirmed by `testdata/canonical_edge_vectors_v0.json` |
| T1.3 | Property test corpus | ✅ | `testdata/canonical_corpus_v0.json`: 200 seeded-random payloads (60 arbitrary-bit floats incl. subnormals, 40 ints to 10^30, 50 Unicode strings, 50 nested mixes), generated by `testdata/generate_canonical_corpus.py` against the reference at `d34d5ca3`; regeneration verified byte-reproducible; replayed by `canonical_corpus_test.go` — all 200 digests byte-identical on the first run |
| T1.4 | Extended golden vectors | ✅ | Edge vectors co-generated and hash-pinned (`golden_provenance_test.go`, LF-normalized); the `golden-drift` CI job clones `shepherd-agents/shepherd` at the pinned `d34d5ca3` and fails the build if the shared golden differs from the reference copy — our side is guarded by the pinned-hash test, the upstream side by this job. The vendored files (edge vectors, corpus, store vectors) have no upstream copy; they are guarded by their pinned hashes plus the generators' verified byte-reproducibility |
| T1.5 | Store-level vectors | ✅ | `store_vectors_v0.json` + `store_vectors_test.go`; plan 01 §4 closed by `8c452b7` — the store allocates Python's record/context/frontier identities |
| T1.6 | `ReadPathPrefix` | ✅ | `d039f0c`: implemented as a documented alias of `ReadOwnerPrefix` (plan 01 §2a — Python's protocol member is a pure delegation too); the same change made `Slice` output ordering deterministic |
| T1.7 | Conformance suite port | ✅ | `conformance_test.go`: `runConformance` over a `ConformanceStore` interface; 10 Python cases ported 1:1, 8 run-output descriptor cases N/A pending plan 04 — mapping table in plan 01 §5 |
| T1.8 | Law coverage map | ✅ | `docs/law-coverage.md` maps all 25 laws; 20 gained dedicated tests in `laws_test.go`. Porting found and fixed three real divergences: duplicate causal parents were deduplicated instead of rejected (`resolvedCauses`), witness bodies were digested unvalidated (`ordinaryWitnessPlan`), and causal-closure read order was randomized by a map range discarding the SQL sort (`canonicalFactOrder` — a flake only a repeated run could catch); the only accepted gaps are law 21's projection half and law 23, both plan-02 dependencies |
| T1.9 | `OpMaterialize`/`OpObserve` doc | ✅ | `3202bbe`: documented reserved-until-plan-03; `TestReservedOperationKindsAreUnreachable` pins the behavior |

**Also absent:** `testdata/UPSTREAM.md`, needed by the §13 v1.0 checklist.

### Phases 2–4

**Phase 2a is complete — merged as `698b7f2` (PR #20) and released as `v0.6.0`.**
execution/relations/history schemas and folds, the runtime handles, and the T2a.6 vectors all landed —
measured by `testdata/execution_vectors_v0.json`: the run sequence, the relation sequence and the
effective-history tree were recorded through the real Python handles (the fail sequence through the `schemas` batch builders), and all of them reproduce ID-for-ID through the Go facade.
Plan 02 §7 is closed. The Phase 2a review (PR #20, `1a66496`) landed typed
not-found errors, honest error wrapping, a real `ChildHandle.Snapshot`, and the
`history_sequence` vectors that made the effective-history fold vector-pinned.
Phases 3 and 4 remain ⬜.

**Phase 2b is complete** (2026-10-09; merged as PR #22 at `df43d33`, released
as `v0.7.0`; the nested repin that activates T2b.7's live test merged as PR
#23, released as `sandbox/containerd/v0.1.3` at `57a5329`). Substrates (`Substrate`/`SubstrateRegistry`, Echo, SQLite KV), the
`Materialize` dispatch with the completed-intent ledger, and the
capability-gated `WorkspaceSubstrate` over `Sandbox` are in, measured by
`testdata/materialize_vectors_v0.json`: the request digests and the
declare→materialize record sequences were recorded through the reference
Echo/KV substrates and reproduce ID-for-ID through the Go dispatch.
`OpMaterialize` is now enforced — `TestReservedOperationKindsAreUnreachable`
was inverted exactly as its comment predicted — and the PR review's five
findings were addressed in-tree (plan 03 §8), including the honest-outcome
rule that world-touched failures report `split_state`, never `clean_failure`.
Phases 3 and 4 remain ⬜.

**Phase 2b's harness exists, has been exercised, and its blocker is cleared**
(`sandbox/containerd/live_test.go`, committed in `603fdcf`, env-gated exactly as
T2b.7 specifies). It is emphatically not a scaffold: it found nine defects, then
exposed a tenth (GC-vulnerable snapshots) that unit tests could not reach, and
T0.9 fixed it. **The soak is green**, so a materialization
path built on this backend will not fail nondeterministically under load.

**The containerd state-durability fix is merged and released** (2026-10-10;
PR #25 merged at `ce07f36`, the six review findings — the orphaned pin on
`advance` failure, the silent no-op without a lease manager, and the three
test-coverage gaps — addressed in `3964041`). States are durable by
construction: `Capture` pins each committed layer on a namespace-scoped
`shepherd-states` lease, `Destroy` removes only the sandbox's private active
layer, and `ReleaseState` is the deliberate reclaim path. Released as
`sandbox/containerd/v0.1.4`, tagged at the merge and verified end-to-end from
a clean module (`go get …/sandbox/containerd@v0.1.4` pulls core `v0.7.0`; core
itself is unchanged). The live suite re-run against the condition that found
the defects — fresh namespace, cold image, empty diffIDs cache — is **9/9
green** (2026-10-10), and plan 03's acceptance sequence
(write → capture → destroy → apply → verify) passes on a live daemon for the
first time, via `TestLive_DestroyRemovesActiveLayerKeepsStates`.

### Sequencing consequence

The §4 dependency graph puts a **hard gate** at Phase 1 → Phase 2. Nothing in
Phase 2b (the sandbox/substrate work) should start before v0.5.0, per §2
principle 1. Note the asymmetry: **a `yaah`-side isolated workspace does not need
Phase 1** because it consumes the `Sandbox` interface directly rather than the
trace-mediated `WorkspaceSubstrate`. That distinction is developed in the
companion plan, `yaah/docs/plans/isolated-workspace-activation.md` §7.

**This section is the maintained status record** until the per-task status column
in §14's convention is applied inline below. Keep it in sync at each phase exit.

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
| M0 | **v0.4.1** (patch; supersedes the mis-placed `v0.4.0`) + nested `sandbox/containerd/v0.1.2` | 0 | Sandbox substrate shipped, tagged, consumable; bug batch; CI bootstrap; **daemon path verified by a 12/12 soak** | W1 |
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

**Exit criteria / acceptance** (annotated 2026-10-05 — see §0):
- [x] `go get github.com/buchenberg/shepherd-kernel-go@v0.4.0` resolves and
      builds; nested module resolvable without `replace`.
      *(Verified: `sandbox/containerd/go.mod` has no `replace` and requires
      tagged `v0.4.0`. Note the tag predates the bug batch — §0.)*
- [x] CI green on 3 OSes; nested module compiles on linux, is excluded (via
      build tags) — not broken — elsewhere.
      *(**Verified 2026-10-07: all four jobs green on PR #10**, run
      `37667196360` — `test (ubuntu-latest)` 1m30s, `test (macos-latest)` 2m49s,
      `test (windows-latest)` 2m46s, `containerd adapter (linux)` 1m5s. The
      gofmt gate passed on Windows and macOS, which is the check that matters
      for this file's CRLF history. On "build tags": the nested module is a
      separate Go module that the root module never imports, so it is excluded
      from the 3-OS builds by module separation rather than by tags — the
      original wording was optimistic. Only `live_test.go` carries a tag, and
      only to keep the live suite out of a plain `go test`.)*

      **Note what CI does and does not cover.** The `containerd adapter` job runs
      `go vet` and `go test` only, so the live suite **skips there** — there is no
      daemon on the runner. CI therefore protects the *mechanism* (the unit tests
      assert the lease reaches the snapshotter context) but **not the daemon
      integration**; the 12/12 soak is currently reproducible only by hand.
      Automating it — the ubuntu runner can host a containerd — would be the
      natural next CI task, and is the only thing standing between this backend
      and silent regression.*
- [x] All six bug-batch fixes merged with tests; T0.5 smoke result recorded
      (pass → phase 2b de-risked; fail → containerd findings converted to
      tasks before T2b.7).
      *(Fixes merged ✅ `4203993`. T0.5 ran, **failed 3/5**, root-caused to
      GC-vulnerable snapshots, fixed by T0.9 (`8e9b867`), and **re-soaked 12/12
      green** — so phase 2b is de-risked. The tenth defect became T0.9 rather
      than a Phase 2b surprise, which is exactly what this criterion was for.)*
- [x] CHANGELOG + LICENSE present.
      *(Verified: `89ebeb3`, `a7562d0`.)*
- [x] **Added (T0.8, decided §15):** `v0.4.1` tagged at/after `55b4daf` so that
      at least one core release contains the bug batch (§0 finding).
      *(Done for the core module: `v0.4.1` published at `bee4ca5` after being
      re-pointed from the pre-review `3315b3a`, and it contains the bug batch.)*
- [x] The **nested** module's released version requires the fixed core.
      `sandbox/containerd/v0.1.2` is published at `df47ec9` and requires
      `v0.4.1`; `go get …/sandbox/containerd@v0.1.2` pulls core `v0.4.1` in a
      clean module. `v0.1.1` remains permanently wrong and must not be used.
      *(T0.8b done. Note the criterion was deliberately split into two boxes:
      the original bundled "tag the core release" and "repin the nested module"
      into one, which is exactly how the gap went unnoticed.)*

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
- [x] Corpus + extended goldens + store vectors all pass; CI fails on golden drift.
      *(Corpus: `canonical_corpus_test.go` replays all 200 seeded payloads
      byte-identically. CI drift, both directions: the pinned-hash test fails the build if our copy of any vector file changes, and the `golden-drift`
      job fails it if the pinned Python reference's copy of the shared golden differs from ours.)*
- [x] Conformance suite complete or N/A-documented; law map has no ❌ without rationale.
      *(PR #15: `conformance_test.go` ports 10 cases, documents 8 N/A pending
      plan 04; `docs/law-coverage.md` maps all 25 laws — the only open rows
      are documented plan-02 dependencies, not ❌s.)*
- [x] `-race` green on the 3-OS CI matrix (PRs #12–#18), and the golden-drift gate is active.
- [x] CHANGELOG flags the digest-behavior fix — the Breaking section leads with the digest-behavior change, the duplicate-parent rejection and the witness-body validation, plus the yaah consumer note.
- [x] `v0.5.0` tagged.
      *(Done: annotated tag `v0.5.0` at `fb1c2a9` (the release merge), pushed after verifying `git merge-base --is-ancestor 0bf34f8 v0.5.0` — the last fix is in the tagged history — and confirmed `go get github.com/buchenberg/shepherd-kernel-go@v0.5.0` resolves from a clean module.)*
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

**Status (2026-10-09): complete — T2b.1–T2b.7 ✅. v0.7.0 tagged at `df43d33` (PR #22, CI green, review findings addressed); the nested repin merged as PR #23 at `57a5329`, and `sandbox/containerd/v0.1.3` is tagged there and verified end-to-end from a clean module.**

| ID | Task | Status | Evidence |
|---|---|---|---|
| T2b.1 | `Substrate` interface, `SubstrateRegistry`, `MaterializationResult`/`Receipt`, outcome vocabulary, `ErrUnknownSubstrate` | ✅ | `substrate.go`: the vocabulary is the reference's `success/clean_failure/split_state` (§1 sketch corrected, plan 03 §7.1); `substrate_test.go` covers register/get/unknown |
| T2b.2 | `Materialize` dispatch: owner-path-explicit `MaterializationRequest`, target validation, request digest, `materialization_intents` ledger, replay/conflict semantics, witness-stamped capture append; ordering (append-before-ledger) verified against Python; enforce `OpMaterialize` | ✅ | `materialize.go` + `materialize_test.go` (all eight `test_materialize.py` cases ported 1:1, plus the crash-window, ordinal-cutoff, mixed-substrate, capture-owner and operation-gate cases); ordering matches `materialization.py`; `OpMaterialize` enforced and `TestReservedOperationKindsAreUnreachable` inverted |
| T2b.3 | `EchoSubstrate` + SQLite KV substrate (`kv.sqlite.local.v1`, `shepherd2.kv.put.v1` → `.applied.v1`, separate world-side file per Python's separation) | ✅ | `substrate_echo.go`, `substrate_kv.go`; separation mirrored (own SQLite file, never a table in the trace DB); payload validation is batch-fatal as in Python |
| T2b.4 | `WorkspaceSubstrate` over `Sandbox`: declaration schemas `workspace.file.{write}.v1`, `workspace.exec.v1` (delete omitted — recorded decision, plan 03 §7.2); capability-gated dispatch (git in-place → honest failure; worktree/containerd → full); applied-captures with path digest / exit code / stdout digest | ✅ | `substrate_workspace.go` + `substrate_test.go` fake-sandbox tests: capability gate → `clean_failure` with nothing applied; mid-batch failure → `split_state` with captures and anchors only for the landed records; exec exit code is an observed result, not a failure |
| T2b.5 | Cross-language vectors: Echo + `materialize` capture record IDs vs Python for identical requests | ✅ | `testdata/materialize_vectors_v0.json` (generator `generate_materialize_vectors.py`, byte-reproducible, hash-pinned in `golden_provenance_test.go`): request digests, echo + KV sequences, ledger-replay-across-restart — replayed ID-for-ID by `materialize_vectors_test.go` |
| T2b.6 | README "Materializing recorded intents" section | ✅ | `README.md` — "What You Can Do With It" section + materialization API reference |
| T2b.7 | containerd live integration (linux CI, env-gated `SHEPHERD_CONTAINERD_ADDR`): WorkspaceSubstrate over live daemon — write→capture→destroy→apply→verify | ✅ | `sandbox/containerd/workspace_live_test.go` — committed dormant in PR #22 (the nested module compiles against published core only), activated by the repin to `v0.7.0` in PR #23: build tag dropped, file joins the normal linux graph, runtime-skips without a daemon. The daemon round trip is executed and green: 2026-10-10, 9/9 live tests against a fresh cold namespace on the dev box's daemon (the PR #25 verification, released as `sandbox/containerd/v0.1.4`) |
**Exit criteria**: plan-03 §6 boxes checked ✅; ledger idempotency proven by
crash-window test ✅ (`TestMaterializeCrashWindowReplayIsConsistent`);
v0.7.0 tagged ✅ (`df43d33`, published). The nested
`sandbox/containerd/v0.1.3` tag is published at `57a5329` (the PR #23
merge) and the T0.8b end-to-end check passes — verified from a clean
module, not inferred from the tag list:

```
$ go get github.com/buchenberg/shepherd-kernel-go/sandbox/containerd@v0.1.3
$ go list -m all | grep shepherd
github.com/buchenberg/shepherd-kernel-go                     v0.7.0
github.com/buchenberg/shepherd-kernel-go/sandbox/containerd  v0.1.3
```

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
  *(Superseded in practice by §0, which is currently the maintained status
  record. Apply the inline status column, or point §14 at §0 — do not let both
  drift.)*

---

## 15. Decision note: enforcement vs materialization (R6)

**Status: DECIDED — 2026-10-05 · Owner: buchenberg · Path A, with C as the
designated fallback and B requiring its own plan.** R6 requires a written
decision note before the containment fence is touched; this is that note. The
fence **stands**, and the decision is to stay inside it.

**Decision (summary).** The kernel remains a materialization/reversibility seam.
No Go containment work is authorised. If real confinement is later required, the
answer is **Option C — compose with a verified jail rather than port one** —
unless B is deliberately chosen with its own plan and parity story. The
re-evaluation gate is `plans/05 §4` durability (T4.1–T4.2), which is the gap
`yaah` actually feels today.

### 15.1 The fence, and what it protects

R6 (§11) flags scope creep toward vcs-core parity — *"world values, carriers,
jails"* — as Medium likelihood / **High** impact, with *"PARITY-PLAN §4 is the
fence; any task touching it requires a written decision note reversing a
GAP-REPORT rejection."*

The fence protects decisions that were argued, not merely deferred.
`GAP-REPORT.md:42-49` rejects two approaches with reasons:

- **file-delta materialization** — git is a better materializer, and deltas
  still miss bash side effects;
- **effect-stream replay** — *"bash side effects are not invertible and the
  trace is not a replayable mutation log."*

and the resolution table records *"Scope state model (fold invariant) —
❌ deliberately not ported"*. These are load-bearing (§2 principle 4).

### 15.2 The fork: "sandbox work" means two different projects

The word covers two activities with different scope, different prerequisites,
and different fences.

**Path A — materialization and reversibility (in scope, planned).**
`Sandbox` as a seam for capturing, applying and diffing `WorkspaceState`, plus
declaration→capture materialization of recorded intents. Planned in full:
`plans/03` (`WorkspaceSubstrate`, gated on Phase 1) and `plans/05 §4`
(durability). This is what the kernel's `Sandbox` interface *is*: git in-place and
worktree backends written, containerd backend written, capability gating via
`Capabilities()` + `ErrUnsupported`.

**Path B — OS-level enforcement (fenced).** Actual confinement of untrusted
execution — Seccomp/Landlock/Seatbelt-class syscall denial, network egress
policy, secret injection. In the Python reference this is `shepherd`'s
`vcs-core`: `_seatbelt_containment.py`, `_landlock_containment.py`,
`_containment.py`, `_egress_broker.py`, `_execution_capability.py`, with a
three-part conformance probe and `UnsupportedMayProfileError` refusing rather
than weakening. **None of it is ported, and R6 says it must not be by accident.**

The distinction is not academic. The kernel's own docs concede it:
`sandboxWorkspace.ResolvePath` notes containment is *lexical* (`path.Clean`), so
an in-sandbox symlink can escape the root — *"a weaker guarantee than the host
validator's symlink resolution, and it is bounded by the container"*
(`yaah/internal/tools/workspace_sandbox.go:55-58`). **A `Sandbox` is not a jail.**

### 15.3 Options

| | Option | Cost | Gets you |
|---|---|---|---|
| **A** | **Stay inside the fence.** Finish materialization: `plans/03`, then `plans/05 §4` durability. | Already estimated (~1–2 wk + ~2 wk) | Reversible, auditable, trace-mediated workspace operations; rollback that survives restart |
| **B** | **Reverse the fence for a Go jail.** Port the containment ladder / syscall jails. | Unestimated; realistically a new workstream | Real confinement in the Go stack; removes the `yaah` "plumbed but not active, bounded by the container" caveat |
| **C** | **Compose rather than port.** Keep the kernel jail-free; get enforcement from the substrate (`daytona`-class sandbox, or `shepherd`'s Python path over ACP). | Integration work, no kernel divergence | Enforcement without forking the ABI or diverging from Python |

### 15.4 Considerations bearing on the choice

**For A (and against B):**
- The kernel's value is a **frozen cross-language ABI**. A Go-only jail adds a
  large subsystem with no Python counterpart, so it cannot be vector-pinned —
  and vectors are the arbiter of parity (§2 principle 3). A jail is precisely
  the kind of thing that is *hard* to pin cross-language.
- `plans/03` already delivers the property `yaah` most needs: isolation of
  *effects*, which is what `supervised_task`'s rollback depends on.
- Enforcement is largely orthogonal to the kernel's purpose. The kernel records
  and reverses; a container runtime confines. Conflating them is how R6's High
  impact arrives.

**For B (and against A):**
- "Plumbed but not active" is an unsatisfying resting place for a safety story,
  and Path A does not change it. A user reading `yaah`'s sandbox code may
  reasonably assume confinement that does not exist.
- The seam already exists if it is ever wanted: `SandboxCapabilities` carries a
  `Containment` field (`sandbox.go:24-41`), `ExecRequest`/`ExecResult` are
  modelled on Daytona's toolbox request specifically so a future backend is a
  straight mapping (`sandbox.go:94-98`), and `Containment` is already a typed
  vocabulary (`full|contained|buffered|uncontained`). Adding enforcement is a
  backend concern, not a redesign.

**For C:**
- Avoids the divergence risk entirely and is the only option that gets a
  *verified* jail (shepherd's is the most rigorously probed in this stack:
  liveness + per-root + deny-closed, with the additive-root nesting attack
  closed at bind time).
- Costs an integration boundary, and `shepherd` is alpha with Windows
  unsupported.

### 15.5 Decision and its consequences

**DECIDED: take A now. C is the designated fallback if enforcement becomes a
requirement. B requires its own plan.**

Consequences, in force as of 2026-10-05:

1. **No containment task may be scheduled** against this repo. The fence stands:
   no syscall jails, no egress broker, no containment ladder, no `may=`-style
   grant vocabulary in Go.
2. **`plans/05 §4` durability (T4.1–T4.2) is the re-evaluation gate.** Finish
   trace-based recovery and checkpoint durability, then revisit enforcement with
   the durability question closed. This is also the highest-value work for `yaah`
   today: in-memory checkpoints mean rollback does not survive a restart.
3. **`plans/03` proceeds unchanged.** `WorkspaceSubstrate` is in-fence work and
   remains the plan for the materialization path — still gated on Phase 1 by §4.
4. **Documentation must not overclaim.** Because Path A leaves the
   "plumbed but not active, bounded by the container" caveat in place, README and
   package docs must state plainly that `Sandbox` is a *materialization seam, not
   a containment boundary*, and that `sandboxWorkspace.ResolvePath` containment is
   lexical. Add this to T4.7's doc pass. A user must not be able to read the
   `Sandbox` docs and infer confinement.
5. **If C is ever adopted**, it is an integration, not a fork: the jail stays in
   the substrate (a `daytona`-class sandbox, or `shepherd`'s Python `vcs-core`
   over ACP), and this repo gains no containment surface.

If B is ever proposed, it must arrive with: the Python-side story (permanent
divergence, or a port awaiting upstream?), a non-vector verification strategy to
replace the missing cross-language pin (§2 principle 3), the
`SandboxCapabilities.Containment` semantics, and an explicit statement of what
`yaah` may then claim. Until that exists, no task may touch the fence.

### 15.6 What this note does *not* decide

It does not authorise containment work. It does not change `plans/03`,
`plans/05`, or `PARITY-PLAN §4`, all of which proceed as written. What changes is
that the fork is now closed rather than pending: **Path A is the plan of record**,
and any future move to B re-opens this note rather than silently extending it.
