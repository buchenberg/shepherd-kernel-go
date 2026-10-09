# Plan 03 — Substrates & Materialization Dispatch

**Phase**: 2 (parallel with plan 02) · **Priority**: P1 · **Estimate**: ~1–2 weeks (~600–900 LOC + tests)
**Status**: implemented (2026-10-08) — code and tests landed; see the status notes at §7
**Goal**: Port shepherd2 `vnext` substrate/materialization semantics — typed
declaration→capture substrate dispatch with an idempotency ledger — and bridge
it to the Go-native `Sandbox` abstraction so recorded intents can actually
reach a workspace (git or containerd) through a uniform, auditable path.

**Python anchors** (under `../shepherd2/src/shepherd2/vnext/`):
- `substrates.py` — `Substrate` protocol, `SubstrateRegistry`,
  `MaterializationResult`, `MaterializationReceipt`, `EchoSubstrate`,
  `SQLiteKVSubstrate` (`kv.sqlite.local.v1`; `shepherd2.kv.put.v1` →
  `shepherd2.kv.put.applied.v1`)
- `materialization.py` (~296 L) — `MaterializationRequest` (owner-path
  explicit), `materialize()` dispatch, completed-intent ledger
- `../shepherd2/tests/test_materialize.py`

Note: vNext is **outside the ABI freeze** (`kernel-abi-v0.md`), so we have
latitude on Go shape — but mirror schema refs and the request/receipt record
shapes so traces stay cross-readable.

---

## 1. Substrate interface (substrate.go)

> Status note: implemented as sketched, except the outcome vocabulary — the
> reference's `success | clean_failure | split_state` replaced the sketch's
> guess (§7, decision 1). See `substrate.go` for the landed shape.

```go
type Substrate interface {
    SubstrateRef() string
    DeclarationSchemas() []string // intents this substrate accepts
    CaptureSchemas() []string     // receipts it emits
    Containment() Containment
    Materialize(ctx context.Context, records []Record) (MaterializationResult, error)
}

type MaterializationResult struct {
    Outcome         MaterializationOutcome // succeeded | failed | partially_applied?
    CaptureDrafts   []RecordDraft          // receipt records to append (capture mode)
    FailureReason   string
    WorldSideAnchors []ExternalAnchor      // refs to effects outside the trace
}

type MaterializationReceipt struct {
    RequestDigest string
    CaptureRecordIDs []string
    Outcome MaterializationOutcome
}

type SubstrateRegistry struct{ ... } // Register / Get
var ErrUnknownSubstrate = errors.New(...)
```

`Materialize` receives the **selected declaration records** (already retained
in the store, addressed by the request) and returns capture drafts; it never
appends itself — dispatch owns the append (§2). This mirrors Python's
separation and keeps substrates testable without a store.

## 2. Dispatch + idempotency ledger (materialize.go)

```go
type MaterializationRequest struct {
    AppendIntentID            string
    TargetTraceOwnerID        string   // owner-path explicit — bare record IDs insufficient (Python law)
    TargetRecordIDs           []string // declaration records to materialize
    TargetThroughOwnerOrdinal int
    CaptureTraceOwnerID       string   // defaults to target owner
}

func Materialize(ctx context.Context, store *SQLiteTraceStore, opCtx OperationContext,
                 req MaterializationRequest, reg *SubstrateRegistry) (MaterializationReceipt, error)
```

Semantics to mirror from `vnext/materialization.py`:
- Validate `opCtx.Operation == OpMaterialize` — this is where the currently
  dead operation kind becomes enforced (closes plan 01 §2 item).
- Resolve targets through the owner path (records must exist on
  `TargetTraceOwnerID` at ordinals ≤ `TargetThroughOwnerOrdinal`; declarations
  must advertise a schema in the substrate's `DeclarationSchemas()`).
- Compute `request_digest` = canonical digest of the request; check the
  **completed-intent ledger** (new SQLite table
  `materialization_ledger(append_intent_id PRIMARY KEY, request_digest, receipt_json)`):
  same intent + same digest → replay stored receipt, no substrate call, no new
  records; same intent + different digest → conflict error (reuse
  `AppendIntentConflictError` shape).
- Witness-stamped: the appended capture batch carries the dispatching
  operation context's witness (normal append path — the store auto-plans it).
- Append captures **before** recording the ledger entry; ledger write is the
  commit point for idempotency (crash between append and ledger → replay
  appends are intent-idempotent at the store level, so at-least-once is safe:
  verify this reasoning against Python's ordering and match it).

## 3. Reference substrates

- **EchoSubstrate** (parity): echoes declaration payloads into capture drafts.
  Test double + conformance fixture.
- **KV substrate** (parity): port `SQLiteKVSubstrate` —
  `kv.sqlite.local.v1`, declaration `shepherd2.kv.put.v1`, capture
  `shepherd2.kv.put.applied.v1`; own SQLite file (or table in the trace DB —
  check Python: it is a separate world-side store; mirror that separation).
- **WorkspaceSubstrate** (Go-native bridge, the payoff): implements
  `Substrate` over a `Sandbox`:

```go
func NewWorkspaceSubstrate(sb Sandbox) *WorkspaceSubstrate
// SubstrateRef() = "workspace.sandbox.v1" (+ backend tag, e.g. workspace.sandbox.git.v1?)
// Decision: substrate_ref identifies the KIND; backend goes in the capture payload.
// DeclarationSchemas: consumer-defined write/exec intents, e.g.
//   "workspace.file.write.v1"  {path, content_b64, perm}
//   "workspace.file.delete.v1" {path}
//   "workspace.exec.v1"        {command, args, cwd, env, timeout_ms}
// Materialize: dispatch to sb.WriteFile / sb.Exec gated on
//   sb.Capabilities() (FileIO/Exec false → Outcome=failed, FailureReason=ErrUnsupported);
// captures: "workspace.file.write.applied.v1" etc. with {path, digest, exit_code, stdout_digest}
```

  This gives GitSandbox (diff-only) an honest capability story and makes
  containerd's full FileIO/Exec capabilities reachable *through the trace*:
  an agent (yaah) records declared intents, materialization applies them in
  the sandbox, receipts land as captures — the same declare→capture rhythm as
  the kernel, instead of ad-hoc `PostTool` recording.

## 4. Tests

- `substrate_test.go`: registry register/get/unknown; echo substrate
  declaration→capture pairing; capability gating (fake sandbox with FileIO
  false → clean failure result, no partial append).
- `materialize_test.go`: happy path (ledger entry + capture IDs);
  intent replay (second call → identical receipt, no new records); intent
  conflict (different digest → error); wrong owner path / missing targets /
  ordinal beyond cutoff → errors; non-`materialize` operation context
  rejected; crash-window simulation (append then fail ledger → replay is
  consistent).
- Vector-pinned cross-language test for `EchoSubstrate` + `materialize`
  record IDs using the plan-01 §3 generator (Python produces the same request
  → same capture record IDs), guaranteeing trace cross-readability.
- Containerd-backend integration (Linux CI only, plan 05 §3): workspace
  substrate over a live containerd sandbox — write file → capture → destroy →
  apply state → file present.

## 5. Non-goals

- No scheduling/queueing of materialization (callers invoke `Materialize`).
- No port of the skeleton `Session` (vcs-core dependency).
- No reversibility-tier ordering (single-substrate dispatch; revisit trigger
  in PARITY-PLAN §5).
- `run_outputs` vocabulary stays with plan 04.

## 6. Acceptance criteria

- [x] Echo + KV substrates pass Python-parity vectors.
      (`testdata/materialize_vectors_v0.json`, generated by
      `generate_materialize_vectors.py` against `shepherd2@d34d5ca3`,
      hash-pinned in `golden_provenance_test.go`, replayed ID-for-ID by
      `materialize_vectors_test.go` — declaration ids, produced capture ids,
      receipts, witness bodies, and world-side KV values all reproduce.)
- [x] Ledger idempotency: duplicate requests replay receipts; conflicting
      duplicates fail loudly.
      (`TestMaterializeRetryReturnsReceiptWithoutRedispatchingSubstrate` —
      including the restarted-store case — and
      `TestMaterializeIntentConflictIsRejectedBeforeSubstrateDispatch`,
      both ported 1:1 from `test_materialize.py`. The crash window between
      append and ledger is covered by
      `TestMaterializeCrashWindowReplayIsConsistent`.)
- [x] `OpMaterialize` enforced; `WorkspaceSubstrate` round-trips file writes
      through a fake sandbox.
      (`TestMaterializeRejectsNonMaterializeOperationContext`;
      `TestReservedOperationKindsAreUnreachable` inverted exactly as its old
      comment predicted; the fake-sandbox round trip and the capability gate
      are `substrate_test.go`'s
      `TestWorkspaceSubstrateRoundTripsFileWrites` and
      `TestWorkspaceSubstrateCapabilityGatingFailsCleanly`. The live-daemon
      round trip is §7's pending item.)
- [x] README: "Materializing recorded intents" section.

## 7. Status notes (2026-10-08)

Landed as T2b.1–T2b.6, plus T2b.7's test file. Two decisions made during
implementation, both in the "Python decides, or the deviation is written
down" discipline:

1. **The outcome vocabulary is `success | clean_failure | split_state`** —
   the reference's, read from `vnext/substrates.py`. The §1 sketch's
   "succeeded | failed | partially_applied" was pre-reference guesswork;
   `clean_failure` is what a capability-gated batch returns, and
   `split_state` is what a mid-batch record failure returns.
   `WorkspaceSubstrate` uses all three: capability gate → `clean_failure`
   (nothing applied), mid-batch failure → `split_state` (captures and anchors
   only for the records that landed).

2. **`workspace.file.delete.v1` is not in the vocabulary.** The `Sandbox`
   contract has no delete operation, and the only thing `WriteFile` could
   do — truncate to empty — is not a delete. Recording "delete applied"
   over a truncation would write a trace that lies about the world. When
   the Sandbox interface grows a real delete (a yaah-coordinated change),
   the schema lights up without breaking anything: dispatch selects on
   declaration schema, so an unknown schema already fails loudly.

Other notes:

- The request digest mirrors `_request_digest` byte-for-byte, including the
  ASCII `json.dumps` flavour (the same encoder the store uses for context
  ids) and a `"" ↔ None` mapping for the two Optional fields; the vectors
  pin this with a non-ASCII intent id and a null trust mode.
- The ledger table is `materialization_intents`, Python's name, created at
  store open rather than lazily; a pre-2b database gains it on first open by
  the new code.
- **T2b.7 is activated** (2026-10-09, act 2 of the two-act shape the
  T0.8/T0.8b repin taught): `sandbox/containerd/workspace_live_test.go` was
  committed dormant behind the `shepherd_p2b_live` build tag in the phase-2b
  PR, because the nested module compiles against *published* core only and
  the substrate API first shipped in v0.7.0. Once v0.7.0 was tagged
  (`df43d33`), the repin PR bumped the requirement to `v0.7.0`, dropped the
  extra tag from the file's constraint so it joins the normal linux build
  graph, and the nested release carried it as `v0.1.3`, tagged at `57a5329`
  and verified end-to-end: `go get …/sandbox/containerd@v0.1.3` pulls core
  `v0.7.0` from a clean module. The daemon round trip itself has still not
  been executed — no containerd socket on the dev box; it runs where the
  12/12 soak ran, same as the rest of the live suite.
- The "containerd fake" half of the acceptance criterion is covered at the
  interface level by the core module's `fakeSandbox` tests; the nested
  module's `fakeSnapshotter` suite already owns the adapter lifecycle and
  cannot see the substrate API until the repin (same reason as above).

### 8. PR review findings, addressed (PR #22, 2026-10-09)

The PR review (Copilot) surfaced five findings; all five were addressed in
the review-fix commit:

1. **`int` ordinals cannot carry Python's 2**63−1 on 32-bit targets.**
   `MaxOwnerOrdinal` is now `math.MaxInt` — equal to Python's
   `MAX_OWNER_ORDINAL` on every 64-bit platform (where the vectors run),
   and the largest cutoff this API can express on a 32-bit one. No
   32-bit consumer exists; only the request *digest* of a default-cutoff
   request would differ there.
2. **`clean_failure` results carrying captures were persisted.** Dispatch
   now rejects the combination before the append — the two claims
   contradict each other. Recorded as a deliberate divergence from
   Python, which appends the drafts unconditionally; no Python-pinned
   vector covers the combination, and the reference substrates never
   produce it. Pinned by `TestMaterializeRejectsCleanFailureWithCaptures`,
   including the no-ledger-entry (retryable) property.
3. **World-touched failures were mislabeled `clean_failure`.** A failure
   after a sandbox call was made may have partially applied — backends
   write files in chunks, and an exec can run to completion before failing
   in teardown — so `splitOrClean` now reports `split_state` for any
   world-touched failure, even with zero captures; `clean_failure` is
   reserved for failures that provably touched nothing (capability gates,
   malformed declarations). Pinned by
   `TestWorkspaceSubstrateWorldTouchedFailureIsSplitState`.
4. **`timeout_ms` never reached the containerd backend** (it observes only
   the context). The substrate now derives a deadline-bearing context from
   the declared timeout, so the declaration means the same thing for every
   backend, and rejects negative values before any sandbox call. Pinned by
   the deadline assertions in `TestWorkspaceSubstrateRecordsExecOutcome`
   and `TestWorkspaceSubstrateTimeoutDiscipline`.
5. **The README overstated the replay guarantee.** Rewritten to state the
   actual contract: idempotent once an intent has completed; dispatch is
   at-least-once in the crash window, where deterministic substrates
   converge and non-deterministic ones fail loudly as intent conflicts.
