# Plan 01 — ABI Conformance Hardening

**Phase**: 1 · **Priority**: P0 · **Estimate**: ~1–2 weeks (~1000–1500 LOC incl. tests)
**Goal**: Make the claim "byte-identical digests from the same inputs" a
*tested property over the whole store path*, not just over hand-picked digest
vectors. Close every known Ring-0 divergence from `shepherd2`.

**Python anchors** (all under `../shepherd/shepherd2/`):
- `src/shepherd2/kernel/canonical.py` — canonical rules (`ensure_ascii=False`,
  `allow_nan=False`, CPython float repr)
- `tests/golden/kernel_abi_v0.json` — shared golden file (byte-identical to
  `testdata/kernel_abi_v0.json`, SHA-256 verified 2026-09-16)
- `tests/test_kernel_abi_v0.py`, `tests/test_kernel_abi_v0_laws.py` (25 laws)
- `tests/test_trace_store_conformance.py` (438 L, backend-agnostic, designed
  to gate a Go backend)
- `src/shepherd2/kernel/facts.py` — `TraceStore` protocol

---

## 1. Canonical JSON byte-identity (canonical.go)

Two known divergence classes between Go `json.Marshal` and Python
`json.dumps(sort_keys=True, separators=(",",":"), ensure_ascii=False, allow_nan=False)`:

1. **HTML escaping.** Go escapes `<`, `>`, `&` as `\u003c`, `\u003e`, `\u0026`;
   Python emits them raw. Any payload containing these (file paths with `&&` in
   bash commands, `<placeholder>` text) yields different digests.
2. **Float formatting.** Go renders integral floats via `%d` (`0.0` → `"0"`,
   `-0.0` → `"-0"`); Python renders `"0.0"`, `"-0.0"`. Exponent-form thresholds
   also differ (CPython switches to scientific at `>=1e16` / `<1e-4`; Go's
   shortest-`%g` switches at `<-4` / `>21`).

**Work**:
- Replace the `json.Marshal` string path in `writeCanonicalValue` with a custom
  string escaper (or `json.Encoder` + `SetEscapeHTML(false)`) matching Python:
  raw UTF-8 passthrough, `\"` `\\` `\b` `\f` `\n` `\r` `\t`, `\u00XX`
  (lowercase hex) for other control chars; lone surrogates → error (Python
  `ensure_ascii=False` still encodes them permissively, but our payloads come
  from `json.Unmarshal` which rejects them anyway — assert and document).
- Implement `formatCanonicalFloat(f float64) (string, error)` replicating CPython
  `float_repr`: shortest round-trip digits (`strconv.FormatFloat(f, 'e'/'f', -1)`
  machinery), fixed notation for `1e-4 <= |f| < 1e16`, scientific otherwise with
  Python's exponent shape (`1e+16`, `1e-05`); `-0.0` preserved; `NaN`/`±Inf` →
  explicit error (mirrors `allow_nan=False`).
- Numbers arriving as `json.Number` continue to render verbatim (they are
  already canonical text from a JSON source) — but validate they round-trip.
- Map keys: require string keys; error on anything else (Python coerces
  int/float/bool/None keys; document the restriction — payloads in practice are
  string-keyed JSON).

**Tests** (`canonical_test.go`):
- Table-driven Python-vs-Go cases: `0.0`, `-0.0`, `2.5`, `1e16`, `1e-7`,
  `0.1+0.2`, `9007199254740993`, `"<script>a&&b</script>"`, `"ünicode"`,
  control chars, nested structures mixing all of the above. Expected values
  generated **by Python** (script in §3), never by Go.

## 2. Store-protocol gaps (store.go / types.go)

- **Add `ReadPathPrefix(ctx ReadContext, path PathPrefix, through int, modeFilter ModeFilter) (Slice, error)`**
  — the only missing `TraceStore` protocol method (`path_entries` table already
  exists). Mirror `read_path_prefix` semantics: owner-agnostic path addressing,
  same slice pipeline as `ReadOwnerPrefix`.
- **Fix `ExternalAnchor.AnchorKind`** — never populated; `anchorForFact`
  callers pass `"outside_frontier"` into `HiddenReason` position only
  (store.go ~1034/1098). Decide the vocabulary from Python
  (`facts.py` anchor shapes) and populate both fields.
- **`ReadOwnerCutoff` signature** — Python protocol passes context; Go takes
  none. Add a ctx-taking method; keep the old one as a wrapper until plan 05's
  `context.Context` sweep.
- **`OpMaterialize` / `OpObserve`** — document as reserved-until-plan-03; add a
  test asserting they are currently rejected/ignored consistently rather than
  silently trusted.

### 2a. Status, corrected against the Python reference (2026-10-07)

Three of the four items above were wrong when written. Verified by reading
`../shepherd/shepherd2/src/shepherd2/kernel/facts.py` and `trace_store.py`:

| Item | Verdict |
|---|---|
| `ReadPathPrefix` | **Real gap**, but the description was wrong. It *is* in the protocol (`facts.py:443`), however its body is a pure delegation — `return self.read_owner_prefix(read_context, trace_owner_id, through, mode_filter)` — and it takes a **trace owner id, not a free-form path**. There is no "owner-agnostic path addressing" distinct from `ReadOwnerPrefix`. Implemented as a documented alias. |
| `ExternalAnchor.AnchorKind` | **Already fixed.** Set in `anchorForFact` (both its early-return and success paths) by the Phase 0 bug batch (`4203993`, shipped in `v0.4.1`) — the original note described the pre-fix state. Referenced by symbol, not line: the line numbers this note first cited had already drifted by the time it was reviewed. |
| `ReadOwnerCutoff` signature | **The premise is false.** Python's protocol member is `def read_owner_cutoff(self, frontier_id: FrontierId) -> OwnerCutoff`, taking **no context**. Go already matches Python. No change needed. |
| `OpMaterialize` / `OpObserve` | Still open. |

### 2b. Found while implementing: read-result ordering was nondeterministic

Implementing the alias surfaced a genuine ABI divergence the plan did not
anticipate.

`Slice.FactIDs()` flattened `OwnerPaths` by ranging over a Go **map**, so the
order changed on every call. Python's equivalent is deterministic:
`TraceSlice.owner_paths` is a `dict[TraceOwnerId, tuple[FactId, ...]]` and
`fact_ids()` flattens it — and dict order is insertion order. So Go disagreed with
Python *and with itself*: the first run of the new alias test failed with the same
fact IDs at different positions on two consecutive reads.

The same defect applied to `ExternalAnchors`, `ContextAnchors` and
`WitnessAnchors`, all built via `mapToSlice`, which ranged over a map. Python holds
all three as dicts (`trace_store.py:646-649`) and converts to tuples in insertion
order.

Fixed by recording first-insertion order for each collection and emitting through
`orderedValues`, with a sorted-key fallback so a missed insertion site degrades to
a *different deterministic* order rather than to nondeterminism. `Slice` gains
`OwnerPathOrder`. Pinned by `TestSliceOutputOrderIsDeterministic`, which asserts 20
consecutive reads agree — a single read always looks correct, which is why this
survived.

**Worth generalising:** any Go field backed by a `map` and compared against Python
is suspect, because Python's dicts are ordered and Go's maps are not. Where a
Python structure is a dict or tuple, the Go port needs an explicit order.

## 3. Extended golden vectors (co-generated with Python)

Extend the **shared** file (`testdata/kernel_abi_v0.json` ≡
`shepherd2/tests/golden/kernel_abi_v0.json`) with new vector groups covering
§1's edge payloads:
- `canonical_edge_floats[]`, `canonical_edge_strings[]` — each
  `{value, canonical_bytes, digest}` produced by `canonical_json_bytes` /
  `canonical_digest`.
- **Process**: add a generator script in the Python repo
  (`shepherd2/tests/golden/generate_edge_vectors.py` — a Python-side
  contribution, note in the PR), regenerate, copy the file into `testdata/`,
  and add a CI hash-compare step (plan 05 §8) so drift fails the build.
- If the Python repo cannot take the change promptly: vendor the generated
  file here with `// generated-by: shepherd2@<commit>` provenance comments and
  the hash check against the cloned reference pinned in CI.

## 4. Store-level cross-language vectors

> ### ✅ §4 CLOSED: the store now allocates Python's identities
>
> Measured again on the same fixture — `intent:a` / owner `exec:one` /
> `draft(kind="step", mode=capture, schema_ref="shepherd2.conformance.step.v1",
> payload={"value":1})` with the trusted internal append context:
>
> | identity | Python | Go |
> |---|---|---|
> | record id | `sha256:2dc99e3f6954d51b…` | `sha256:2dc99e3f6954d51b…` ✅ |
> | witness ref | `sha256:160548ff3f251949…` | `sha256:160548ff3f251949…` ✅ |
> | context id | `context:445f12c02f7e458fd07811dc7df7fc47` | `context:445f12c02f7e458fd07811dc7df7fc47` ✅ |
> | commit receipt | `commit:0` | `commit:0` ✅ |
> | owner ordinal range | `(0, 0)` | `(0, 0)` ✅ |
>
> `testdata/store_vectors_v0.json` now replays nine fixtures from the reference
> store and asserts ID-for-ID equality, so this is verified rather than asserted.
> The four causes were:
>
> 1. **The context id was a different algorithm.** Python is
>    `f"context:{sha256(f'{intent}\\0{group_index}\\0{json}').hexdigest()[:32]}"`
>    over the context payload alone, encoded `ensure_ascii=True`. Go hashed a JSON
>    object of `intent`/`group_index`/`payload` with `json.Marshal` and prefixed
>    `ctx:` with all 64 hex characters — different input shape, prefix and length.
>    All three had to change.
> 2. **Two JSON encoders, and Go had one.** Record and witness ids use
>    `canonical_json_bytes` (`ensure_ascii=False`), but `_json_dumps`
>    (`trace_store.py:1519-1520`) omits that argument, so the context id, the batch
>    digest, stored `body_json`/`caused_by_json` and `receipt_json` all use
>    `ensure_ascii=True`. `canonicalJSONASCIIBytes` now implements the second
>    flavour: non-ASCII as `\\uXXXX`, surrogate pairs above U+FFFF, DEL escaped, and
>    — unlike `encoding/json` — no HTML escaping of `<`, `>` or `&`.
> 3. **The witness plan took `authority_refs` from the wrong source.** Go used the
>    append context's `presented_witness_refs`; Python uses the *retained context's*
>    `capability_witness_refs`. That tied the witness id, and every record id citing
>    it, to the caller's credentials — so identical content appended by a different
>    trusted caller produced a different record id. The root witness's `kind_label`
>    was also `root_witness` where Python has `witness_root`.
> 4. **Witness insertion order was map-random.** Witness plans were collected by
>    ranging a Go map, and that order is what assigns the witness records their
>    owner ordinals. Python's `witness_plans` is an insertion-ordered dict
>    (`trace_store.py:480-481`), root first, then each group's ordinary witness. Two
>    identical appends could therefore allocate different ordinals, and the
>    `kernel:witness` owner path read back in a different order each time. **The
>    store-level vectors caught this one** — no digest-layer vector could have.
>
> The batch digest was aligned in the same pass: it previously marshalled Go structs,
> so the JSON carried Go field names and declaration order rather than Python's
> sorted snake_case keys (`atomicity`, `schema_version_set`, snake_case group and
> draft fields, `reuse_context_id`). Since the conflict check is part of the ABI, a
> batch one store rejected as a conflicting reuse the other would have accepted.
>
> One residual difference, recorded here rather than hidden: Python's
> `append_local_id` is `None` when unset while Go uses `""`, so `""` maps to JSON
> `null`. An explicitly empty `append_local_id` would therefore differ, and no port
> of a working caller can currently produce one.

Digest-layer identity does not prove the *store* is identical — witness plan
construction, retained-context hashing (`ctx:<sha256>`), frontier records, and
commit-receipt shapes could still diverge.

- Define fixture batches in a new shared file
  `testdata/store_vectors_v0.json` (Python-generated): for each fixture —
  `append_intent_id`, groups (owner paths, retained contexts, causal parents,
  local refs), operation context — record the resulting **record IDs** (user +
  auto witnesses), context IDs, frontier IDs, and owner ordinals produced by
  `SQLiteTraceStore.append` against a fresh DB.
- Go test: replay each fixture through `SQLiteTraceStore.Append` /
  `PublishFrontier` and assert ID-for-ID equality.
- Include at least: root witness auto-plan, ordinary witness content for a
  non-default `AppendContext`, retained-context reuse-by-ID, multi-group
  atomic batch, local-ref resolution, frontier publish (record + index), and
  intent replay (same receipt, no new records).

## 5. Conformance suite port

Translate `test_trace_store_conformance.py` (parametrized over a store
factory) into `conformance_test.go`:
- Structure as `runConformance(t, factory func(t *testing.T) *SQLiteTraceStore)`
  so any future backend (plan 03 substrates, in-memory test store) gets gated
  by the same suite.
- Enumerate every Python conformance case first (implementation-time task:
  produce a checkbox mapping table in the PR description, Python test name →
  Go test name or "N/A + reason").

## 6. ABI law coverage map

`test_kernel_abi_v0_laws.py` pins 25 executable laws. Produce
`testdata/law-coverage.md` (or a table in the PR): law → covering Go test.
Known-covered (from existing Go tests): frozen markers, record_id==digest,
kind_label exclusion, witness retention/validation, empty-witness sentinel,
ordered-parent identity, shape_only hiding + context anchors, witness anchors,
full_internal authority, cut immutability, restart persistence.
Likely gaps to add: duplicate causal parent rejection, append-local refs
resolved pre-retention, witness cycle rejection, witness support closure
dedupe/mode-filter-independence, closure policy `include_external_anchors` vs
`visible_only` anchor behavior, mode filter not pruning traversal,
operation-context non-cross-authorization for cut publication, projection
incompatibility (lands with plan 02's schema library — note dependency).

## 7. Execution order

```
1. §1 canonical escaper + float formatter (+ edge tests w/ Python-generated values)
2. §3 extend shared golden file; wire hash-compare check
3. §4 store-level vectors + replay test
4. §2 ReadPathPrefix, AnchorKind, ReadOwnerCutoff ctx
5. §5 conformance port; §6 law map, add missing law tests
6. Full suite: go test ./... -count=1 (both modules)
```

Steps 1–2 are the trust foundation; 4 can run in parallel by a second
contributor. Do not start plan 02's schema ports until step 1 lands (schema
IDs are digests — a canonical drift there is expensive to unwind).

## 8. Acceptance criteria

- [ ] Payloads containing `<`, `>`, `&`, floats (`0.0`, `-0.0`, `1e16`, `1e-7`)
      produce digests identical to Python `canonical_digest` (Python-generated
      vectors in-tree).
- [ ] Shared golden file hash matches the Python repo's copy; CI fails on drift.
- [ ] `store_vectors_v0.json` replay passes: all record/context/frontier IDs
      byte-identical to Python store output.
- [ ] Every Python conformance case either ported or documented N/A.
- [ ] 25-law coverage table complete; all laws pass or have documented
      deliberate-divergence rationale.
- [ ] `ReadPathPrefix` implemented + tested; `AnchorKind` populated.
