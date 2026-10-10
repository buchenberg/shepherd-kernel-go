# Changelog

Notable changes to `shepherd-kernel-go`. This project follows
[Semantic Versioning](https://semver.org/); while pre-1.0, minor releases may
contain breaking changes, which are called out below.

## [sandbox/containerd/v0.1.5] - 2026-10-10

The nested module repins to core `v0.10.0` (from `v0.7.0`), which is what
carries the sandbox registry and the settlement surface — until this release,
importing the adapter alongside a current yaah pin would silently compile
against pre-ctx core. Core itself is unchanged and stays at `v0.10.0`;
`go get …/sandbox/containerd@v0.1.5` pulls core `v0.10.0` from a clean module.

### Added (containerd)
- **`ApplyDelta` — the containerd backend now implements
  `shepherd.DeltaApplier`**, so a sealed containerd output can settle through
  `SettleApply` onto a parent that moved since the fork, the verb yaah's
  isolated-workspace plan reaches for in its apply-out.
  - `Apply` records the applied state's git HEAD as the sandbox's delta
    base; every `Capture` from then on carries, in the state's `Data`,
    `delta_base` plus `delta_patch` — a base64 git binary patch from the base
    to the captured working tree, untracked files included. A state from a
    sandbox that never applied a git state carries neither key and settles
    `SettleSelect`-only; `ApplyDelta` on it returns `ErrUnsupported` instead
    of pretending.
  - The merge runs at the tree level in the repository's object database
    (`git merge-tree --write-tree` over the reconstructed delta tree, the
    working tree as it stands, and the base tree), so a conflicting delta
    refuses with the working tree untouched — no conflict markers, no
    partial application — and the refusal names the conflicting paths. A
    clean merge lands as unstaged working tree changes; the caller's staged
    work survives, because the final materialization is a plain `git apply`
    whose preimage is the working tree itself.
  - The patch payload rides the state's `Data`, so it persists through
    `Seal`'s trace record and `RecoverScopes` — a recovered output is still
    settle-able.
- **OpenSandbox self-registration**: importing the package registers the
  backend under `containerd` (the same `init()` convention as the in-core
  git backend), so config-driven selection no longer needs host wiring —
  `shepherd.OpenSandbox("containerd", containerd.Config{…})` resolves it, and
  a host that also calls `RegisterSandbox("containerd", …)` itself must not
  (duplicate registration panics, by the registry's design).

### Changed (containerd)
- **The module pins core `v0.10.0`** (from `v0.7.0`), which carries the
  ctx-first store APIs; the module's own live tests were swept accordingly
  (`store.Close/Append/ReadFact` now take the context). The adapter's own
  API is unchanged.
- `WorkspaceState.Data` gained two keys (`delta_base`, `delta_patch`); states
  are otherwise unchanged and apply as before.

> Everything in this section shipped 2026-10-10 as the nested-module release
> `sandbox/containerd/v0.1.5`, tagged at `974bc56` (the PR #33 merge); core
> stays at `v0.10.0`. Verified end-to-end from a clean module:
> `go get …/sandbox/containerd@v0.1.5` pulls core `v0.10.0`. The live suite
> on the final commit is green — 11/11, including both ApplyDelta acceptance
> tests. The release-verify CI job's nested-tag branch failed on this tag's
> own run — it had never executed before (every earlier nested tag predates
> the job's `sandbox/` trigger) and passed the full ref as the `go get`
> version operand, which Go rejects; the branch is fixed (PR #34), so the
> guard applies from the next nested tag on. This tag's resolution is
> verified by the hand run above, whose commands match the fixed branch's. The live acceptance for `ApplyDelta` — merge
> onto a moved parent with uncommitted pre-fork work preserved and staging
> intact, and an add/add conflict refusing with the tree untouched and the
> delta still consumable — is `TestLive_ApplyDeltaMergesOntoMovedParent` and
> `TestLive_ApplyDeltaConflictRefusesUntouched` in
> `sandbox/containerd/live_test.go`.

## [v0.10.0] - 2026-10-10

### Added
- **Sandbox backend registry** — the construction seam for config-driven
  backend selection: `RegisterSandbox(backend, SandboxFactory)`,
  `OpenSandbox(backend, cfg)`, `SandboxBackends()`. The factory's `cfg` is
  opaque to the kernel; each backend defines its own config type
  (`GitSandboxConfig` for the built-in git backend, which now
  self-registers). Unknown names fail with `ErrUnknownSandboxBackend`
  listing the registered backends. `OpenSandbox` does not provision —
  `Create` remains the explicit lifecycle step. A host that never imports
  a backend cannot select it: config can name a backend, only imports can
  provide one.

### Changed (docs)
- README: the containerd backend's framing is now "runs anywhere a
  containerd daemon runs — native Linux, WSL2, or a Linux server". Linux
  is stated as what it is — a runtime requirement of containerd and
  overlayfs — not a build-tag fact (the module cross-compiles; its daemon
  does not run off-Linux). The backend section documents why git is
  in-module and containerd is nested (dependency weight and the test
  carrier, not modularity), and the sandbox registry; containerd
  registration is host wiring until the nested module repins to a
  registry-carrying core.

## [v0.9.0] - 2026-10-10

Durability & idioms (plan 05 §4/§6–8, Phase 4). **Breaking**: every store API
now takes `context.Context` as its first parameter, with the trace-authority
context second. The registry survives restarts.

### Changed (breaking)
- **`context.Context` is the first parameter on every store API** —
  `Append`, `PreviewRecordIDs`, `ReadFact`, `ReadOwnerPrefix`,
  `ReadPathPrefix`, `ReadCausalClosure`, `PublishFrontier`, `PublishCut`,
  `ResolveFrontier`, `ResolveCut`, `ReadOwnerCutoff`, `FactCount`,
  `ContextCount`, `Close` — with the trace-authority context
  (`AppendContext`/`ReadContext`) second: the auth context is ABI-facing
  authority, the Go ctx is execution plumbing. Cancellation and deadlines
  now reach the SQL layer (`BeginTx`, `QueryContext`, `ExecContext`). The
  runtime facade follows: `PublishExecutionFrontier`,
  `ProjectExecutionFromStore`, the relations/effective-history projectors,
  `StartTaskSync`, and the `TaskControl` methods take ctx first.
  `PruneCheckpoints` gains a ctx.
- Scope lifecycle records (fork/merge/discard/halt) run on
  `context.Background()`: a lifecycle event must not be abandoned because a
  caller's deadline expired mid-record.

### Added
- **`RecoverScopes(store, RecoverOptions)`**: rebuild the scope tree,
  terminal states, fork baselines, checkpoint registry, and retained-output
  registry from the durable trace after a restart. Sandboxes are re-adopted
  through `RecoverOptions.Resolver` — the host decides what to re-adopt vs
  orphan-clean; shared children inherit the parent's sandbox; isolated
  children (now recorded as `isolated` in the fork payload) get their own.
  A root that never forked is introduced by its checkpoint/output records,
  recovering as a bare active root.
- **Checkpoint durability**: a `checkpoint_snapshots` blob table in the
  trace database stores the full `WorkspaceState` plus the opaque caller
  snapshot, bounded by a per-store blob limit (`WithCheckpointBlobLimit`,
  default 1 MiB — an oversized snapshot fails the checkpoint). Restore
  consumes the blob. A recovered checkpoint whose recorded revision no
  longer matches the re-adopted workspace is `CheckpointInvalid` — never
  silently restorable. (Revision, not digest: git mints a fresh stash
  commit per capture, so a digest rule would invalidate every dirty-tree
  checkpoint.)
- **Retained-output recovery**: sealed payloads carry the full sealed and
  baseline states and the change list, so recovered outputs settle end to
  end; settled records keep consume-once alive across restarts.
- **`PruneTerminal()`**: unregister merged/discarded scopes in long-lived
  hosts; terminal states stay lookupable until pruned. `PruneCheckpoints`
  also deletes persisted blobs.
- **Fork payload additions** (open-map keys, backwards-compatible for
  readers): `isolated`, `sandbox_backend`, and the full `baseline` state.
- **CI release-verify job**: on tags, a clean-consumer `go get` of the tag
  and a no-`replace` check on both modules (the guard that would have
  caught the unbuildable `sandbox/containerd/v0.1.0`).

## [v0.8.0] - 2026-10-10

Supervision & settlement (plan 04, Phase 3): nothing from a child scope
reaches the parent without passing a review gate, and child results are
sealed as retained outputs settled exactly once. Purely additive to the API
surface; the one behavior-relevant note is that `Fork` now captures the
parent's workspace state as a fork baseline (a git capture per fork — and a
failed capture fails the fork rather than handing out an unreviewable child).

### Added
- **Check-at-commit merge gate**: `ScopeManager.ProposeMerge` diffs a child
  against its fork baseline and classifies the unified diff into
  `ProposedChange`s (`create`/`modify`/`delete`); `CommitMerge` records
  `shepherd.merge.proposed.v1`, runs a `MergeReviewer`, records
  `shepherd.supervisor.decision.v1`, and merges or discards — a denial
  returns `*SupervisorDeniedError` (reason + paths) and never merges.
  Built-in reviewers: `DraftsOnlyReviewer` (the reference
  `drafts_only_supervisor` analogue), `DestructivePathReviewer`.
  `Supervisor.ReviewMerge` presents proposals to the rule engine as
  `merge:proposed` effect events; `InterventionDiscard` gains its first
  emitter.
- **Fork baselines (breaking for fork, deliberately)**: `Fork` on a
  sandboxed parent captures the parent's workspace state and records its
  digest in the `shepherd.scope.forked.v1` payload (additive key); the child
  exposes it via `Scope.Baseline`. Children without a baseline (pure-causal
  forks) are refused by `ProposeMerge` and `Seal`.
- **Consume-once settlement**: `Scope.Seal` freezes a child's captured
  workspace plus its classified changes into a `RetainedOutput` and records
  `shepherd.run_output.sealed.v1`; the `ScopeManager` registry
  (`Seal`/`Output`/`OutputsForScope`/`Settle`) settles exactly once —
  `selected` (fast-forward-only: parent must digest-match the fork
  baseline), `applied` (three-way via the new optional `DeltaApplier`
  sandbox capability; path overlap → `ErrApplyConflict`, unconsumed),
  `released`, `discarded`. Second settle → `ErrOutputConsumed`.
- **`DeltaApplier`** optional `Sandbox` capability, implemented by the git
  backend (stash-apply across the shared object store): merge a captured
  state's changes onto a workspace that moved past the state's base.
- The three reference recipes as executable documentation
  (`examples_test.go`): best-of-n, retry-until-acceptable, apply-onto-moved-
  workspace.

### Changed
- `CommitMerge` refuses isolated-sandbox children: `Merge` is causal-only,
  so approving one would record the approval and then destroy the worktree
  holding the approved work. Settlement owns physical propagation for
  isolated children; the error says so.

> Everything in this section shipped 2026-10-10 as the nested-module release
> `sandbox/containerd/v0.1.4`, tagged at `ce07f36` (the PR #25 merge). Core is
> unchanged and stays at `v0.7.0`; `go get …/sandbox/containerd@v0.1.4` pulls
> core `v0.7.0` from a clean module. The cold-namespace live suite — the
> condition that found both defects — is 9/9 green on this code.

containerd backend (`sandbox/containerd/`): captured states are now durable
across sandbox teardown, and a cold-daemon defect in image resolution is fixed.
Both were found by the first live run against a cold daemon (fresh namespace,
image not yet pulled) — the warm-daemon runs that produced "12 consecutive
green" had masked both.

### Fixed (containerd)
- `RootfsSnapshot` passed the caller's raw context to `img.RootFS`, the one
  containerd call in the adapter not wrapped with the namespace injector. On a
  cold daemon the image object comes from `Pull` with an empty diffIDs cache,
  so the call reached the daemon and was rejected with
  `namespace is required: failed precondition`; on a warm daemon
  `GetImage`+`IsUnpacked` (namespaced) had already populated the cache, so the
  missing wrapper never fired.

### Changed (containerd, breaking for direct users of the adapter)
- **Captured states now outlive the sandbox that captured them**, per the
  kernel contract that a `WorkspaceState` has no lifecycle. Every `Capture`
  pins its committed layer as a lease resource on a namespace-scoped
  `shepherd-states` lease; `Destroy` releases the sandbox lease and removes
  only the sandbox's private active layer, never committed layers; `Apply` no
  longer prunes layers captured after the applied state — they are states the
  caller may still hold. Plan 03's acceptance sequence (write → capture →
  destroy → apply → verify) passes for the first time.
- A failed state pin fails the `Capture` and rolls forward onto the committed
  layer as an internal layer (the commit already renamed the active key), so
  the sandbox stays usable and no unpinned state is handed out.

### Added (containerd)
- `StateReleaser` optional interface with `ReleaseState(ctx, state)`: the
  deliberate reclaim path for a state's pin. The daemon's GC reclaims the
  layer once nothing references it. Deliberately not part of core's `Sandbox`
  interface; promoting it is a kernel-API decision.

## [v0.7.0] - 2026-10-08

Phase 2b (substrates & materialization): recorded intents are now actionable —
declarations can be materialized through typed substrates, with the receipts
appended as captures. Ported from shepherd2's vNext layer with the same
request/ledger semantics, plus a Go-native `WorkspaceSubstrate` that bridges
the kernel's `Sandbox` interface so intents reach a real workspace (git,
worktree, or containerd) through the declare→capture rhythm. Purely additive
to the Go API surface; the one behavior change is that `OpMaterialize` is now
enforced (previously reserved and unreachable).

### Added
- Substrate contract: `Substrate` interface, `SubstrateRegistry` (fail-closed
  register/get, `ErrUnknownSubstrate`), `MaterializationResult` /
  `MaterializationReceipt`, and the reference's outcome vocabulary
  (`success` / `clean_failure` / `split_state`).
- `Materialize` dispatch (`materialize.go`): owner-path-explicit
  `MaterializationRequest`, witness-stamped substrate selection, single-substrate
  batches, capture drafts validated against the substrate's capture schemas,
  append-before-ledger ordering, and a `materialization_intents` completed-intent
  ledger that replays stored receipts on retries and rejects same-intent
  different-content requests with `AppendIntentConflictError`.
- `OpMaterialize` is enforced: `Materialize` requires an operation context
  built for it and the trusted internal witness; the previously reserved kind
  is live. `OpObserve` remains reserved.
- Reference substrates: `EchoSubstrate` and the SQLite KV substrate
  (`kv.sqlite.local.v1`; `shepherd2.kv.put.v1` → `shepherd2.kv.put.applied.v1`),
  the latter keeping its world-side database separate from the trace store.
- `WorkspaceSubstrate` (`workspace.sandbox.v1`): file-write and exec intents
  applied through a `Sandbox`, gated on its capabilities — a backend without
  the capability fails cleanly with nothing applied. Any failure *after* a
  sandbox call was made reports `split_state` (captures and anchors only for
  the records that verifiably landed), because backends write files in
  chunks and the world may have changed without a capture; `clean_failure`
  is reserved for failures that provably touched nothing. An exec's exit
  code is an observed result, not a failure, and a declared `timeout_ms`
  is enforced through the context so it reaches every backend.
- Dispatch rejects a substrate result that claims `clean_failure` while
  emitting capture drafts — the two claims contradict each other, and
  Python (which appends them unconditionally) is deliberately diverged from
  here (plan 03 §7).
- `testdata/materialize_vectors_v0.json` (generated by
  `testdata/generate_materialize_vectors.py` against `shepherd2@d34d5ca3`,
  byte-reproducible, hash-pinned): request digests, echo and KV record
  sequences, and the ledger-replay-across-restart case, all replayed
  ID-for-ID by the Go dispatch.
- Containerd live integration test
  (`sandbox/containerd/workspace_live_test.go`), committed dormant behind a
  build tag until the nested module repins to this release (the v0.1.3
  release carries it).

### Changed
- `TestReservedOperationKindsAreUnreachable` inverted into an enforcement
  check, exactly as its own comment predicted plan 03 would.
- `workspace.file.delete.v1` is deliberately not in the workspace vocabulary:
  the `Sandbox` contract has no delete, and truncating a file while recording
  "delete applied" would write a dishonest trace (plans/03 §7).

## [v0.6.0] - 2026-10-07

Phase 2a (schema rings & runtime handles): traces written by the Go kernel are
now interpretable without the Python reference at hand — the execution
lifecycle, relations, effective-history, and schema-library layers are ported,
with a synchronous task facade on top. Purely additive: no breaking changes, no
new dependencies (`go.mod` untouched).

### Added
- Execution lifecycle schema: `shepherd2.execution.{created,started,completed,failed}.v1`
  records, deterministic `exec:<32hex>` ids (`ExecutionIDFor`), batch builders
  (`CreateExecutionBatch`, `CompleteExecutionBatch`, `FailExecutionBatch`),
  `PublishExecutionFrontier` with the terminal-frontier law, and the
  `ProjectExecution(FromStore)` fold.
- Execution relations: `shepherd2.execution_relation.created.v1`
  (`spawned`/`adopted`/`abandoned`), parent-owner-path invariants, and the
  `ProjectExecutionRelations(FromStore)` projection.
- Effective history: `ProjectEffectiveHistory(FromStore)` folding published
  facts and active relations into the `EffectiveHistory` tree.
- Schema library: `ProjectionSpec`, `StaticSchemaLibrary`,
  `EnsureProjectionCompatible`, and the default `ShepherdSchemas()` registry —
  closes the plan-01 law 21/23 gaps (`TestProjectExecutionRequiresBothModes`).
- Runtime handles: `StartTask`/`StartTaskSync`, `Run` and `ChildHandle`
  (Wait/Snapshot/Cutoff), and `TaskControl`
  (CausalTail/Publish/Spawn/Adopt/Abandon/AwaitTerminal/ReadExecution),
  mirroring Python's owner-path convention.
- `testdata/execution_vectors_v0.json`: run, fail, relation, and
  effective-history sequences recorded through the real Python handles
  (`testdata/generate_execution_vectors.py`), replayed ID-for-ID through the
  Go facade, and hash-pinned in `golden_provenance_test.go`.

### Verified
- `TestStartTaskSyncMatchesPythonVector` reproduces the Python `@task` run's
  owner path fact-for-fact through the facade — vector-pinned, not asserted.
- Terminal-frontier and projection-purity laws enforced by negative tests;
  foreign-schema facts and unknown relation kinds are rejected.
- The task-tree integration (`handles_integration_test.go`) exercises
  spawn + adopt + abandon + publish, pending → running → succeeded
  transitions, the fail path, and the restart law: reopen the store file and
  re-project identically.

## [v0.5.0] - 2026-10-07

Phase 1 (ABI trust): the canonical digest layer is now byte-identical to
CPython, proven by Python-generated vectors rather than assertion, and the
store allocates the same identities the Python reference store does. **This
release contains a digest-behavior change** — read on before upgrading.

### Breaking: digest behavior and rejected inputs
- The canonical writer is hand-rolled to match CPython's
  `json.dumps(sort_keys=True, separators=(",",":"), ensure_ascii=False,
  allow_nan=False)` exactly. Two classes of payload now hash **differently —
  and correctly — than in v0.4.1**:
  - payloads containing `<`, `>`, `&`: `encoding/json` HTML-escaped them
    (`\u003c`, `\u003e`, `\u0026`), Python does not;
  - non-integer floats: CPython repr semantics — `0.0` and `-0.0` keep their
    decimal point, shortest round-trip digits, fixed notation inside
    `1e-4 ≤ |x| < 1e16`, scientific outside (`1e+16`, `1e-05`), `NaN`/`±Inf`
    rejected. The old writer rendered integral floats as integers, so `1.0`
    digested as `1`.
  Old traces remain readable — retained records are looked up by id, not
  re-derived. Only newly appended records hash differently. Traces written by
  **≥ v0.5.0 are digest-compatible with Python `shepherd2`**; traces written by
  older Go versions with these payload classes are not (consumers pinned to
  cross-language identity — e.g. `yaah` — should note this in their release
  notes).
- A causal parent cited at both the group and the draft level is now
  **rejected** (`duplicate causal parent`), not silently deduplicated. The
  dedupe changed the parent tuple a record digests over, so identical caller
  input could produce a different record id depending on which level the
  caller used. Python rejects it; so does this.
- A group context with an empty `substrate_ref` or an unknown `containment`
  is now **rejected on append and on preview** (`ValidateWitnessBody` runs
  before the witness digest), instead of being retained as a witness whose
  body the kernel's own schema rejects.
- Payloads decoded from JSON must be decoded with `json.Decoder.UseNumber()`
  (or into `json.Number`): the JSON `1` vs `1.0` distinction is now
  load-bearing in digests. `CanonicalJSONBytes` documents the contract.

### Fixed (store identity — found by replaying Python store vectors)
- The retained-context id now uses Python's algorithm and shape
  (`context:` + 32 hex over the context payload, `ensure_ascii=True`), the
  batch digest uses Python's sorted snake_case key order, the ordinary witness
  plan takes `authority_refs` from the **retained context's**
  `capability_witness_refs` rather than the caller's presented witness refs,
  the root witness carries Python's `witness_root` kind label, and witness
  plans insert in Python's order (root first) rather than map-random order.
  Without these, identical appends produced different record/context/frontier
  ids than the reference store.
- Read results are deterministic: `Slice` flattens owner paths and anchors in
  first-insertion order (`OwnerPathOrder`), and causal-closure reads keep the
  SQL sort instead of ranging over a Go map, which had randomized the order on
  every process run — a single run always looked correct.

### Added
- `ReadPathPrefix`: the last missing `TraceStore` protocol member, a
  documented alias of `ReadOwnerPrefix` (it is one in Python too).
- `OpMaterialize`/`OpObserve` documented as reserved until plan 03, with a
  test pinning that they are unreachable rather than silently trusted.
- Test infrastructure, all oracles Python-generated and hash-pinned
  (`golden_provenance_test.go`):
  - `conformance_test.go` — `runConformance`, the backend-agnostic port of
    `test_trace_store_conformance.py` (10 cases 1:1; 8 run-output descriptor
    cases deferred to plan 04 with the schema they exercise);
  - `laws_test.go` + `docs/law-coverage.md` — all 25 ABI laws mapped, 20 with
    dedicated tests;
  - `testdata/canonical_edge_vectors_v0.json` (42 edge vectors) and
    `testdata/canonical_corpus_v0.json` (200 seeded-random payloads including
    subnormals and both float-notation boundaries) — the canonical writer
    reproduces every byte and digest;
  - `testdata/store_vectors_v0.json` — store fixtures replayed ID-for-ID.

### Verified
Every parity claim in this release is measured against a Python-generated
fixture: 42 edge vectors, 200 corpus payloads and 9 store fixtures all
reproduce byte-for-byte, from the reference at `shepherd2@d34d5ca3`. The
conformance suite and the 25-law map gate the store protocol; `-race` is
green across the 3-OS CI matrix.

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
- **The containerd sandbox now holds a lease for its lifetime.** Without one a
  snapshot is referenced only by the container rooted at it, and `Capture` stops
  that container before preparing its successor — leaving the just-committed
  layer unreferenced for the daemon's garbage collector to reclaim. That made the
  live suite flaky, failing **3 runs in 5** with a different test each time. The
  sandbox now acquires a labelled lease in `Create`/`Apply` and releases it in
  `Destroy`. Verified by **12 consecutive green runs** and by a live assertion
  that the lease lists the sandbox's own snapshot keys as resources.
- **Lease lifecycle hardened after review:**
  - Acquisition and release are serialized. Without that, two concurrent
    `Create`/`Apply` calls could both create a lease and overwrite one id, and
    the loser would never be released — pinning the sandbox's snapshots against
    the GC for good, which is the failure the lease exists to prevent.
  - A failed lease delete keeps its id, so a later `Destroy` retries instead of
    reporting success over an orphan that holds snapshots indefinitely.
  - `SandboxSpec.Timeout` now bounds the daemon connection and the lease RPC, not
    only the work that follows them.
  - `Diff` stages once and derives both its file list and its unified diff from
    that single pass. Two stagings could describe different workspace states if
    the workspace changed between them.

### Verified
The containerd backend's daemon path is exercised, not merely compiled. The live
harness (`sandbox/containerd/live_test.go`) covers full lifecycle, namespace
isolation, destroy keeping the image rootfs, diff preserving pre-staged changes,
stdin rejection, lease-held snapshots, and lease release on destroy. It still
needs a Linux host with a running containerd daemon — a stock image and the
overlayfs snapshotter are enough, no root-owned FIFO directory required.

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
