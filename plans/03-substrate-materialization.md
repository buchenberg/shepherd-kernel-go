# Plan 03 — Substrates & Materialization Dispatch

**Phase**: 2 (parallel with plan 02) · **Priority**: P1 · **Estimate**: ~1–2 weeks (~600–900 LOC + tests)
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

- [ ] Echo + KV substrates pass Python-parity vectors.
- [ ] Ledger idempotency: duplicate requests replay receipts; conflicting
      duplicates fail loudly.
- [ ] `OpMaterialize` enforced; `WorkspaceSubstrate` round-trips file writes
      through containerd fake and (Linux CI) live daemon.
- [ ] README: "Materializing recorded intents" section.
