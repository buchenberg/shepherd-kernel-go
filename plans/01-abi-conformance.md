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

### ✅ §5 CLOSED: `conformance_test.go` ports the suite

`runConformance(t, open)` runs every case as a subtest named after the Python
test (minus the `test_` prefix), against a `ConformanceStore` interface trimmed
to the members the suite exercises — a future backend is gated by implementing
that interface, not by being a `*SQLiteTraceStore`. The harness binds each
case to one directory, created once, and hands it to the factory on every
open, so the restart case reopens the same durable store exactly as the
Python fixture does. (The first version derived the location inside the
factory from `t.TempDir()`, which returns a NEW directory on every call — so
the restart case was passing against a fresh, empty store, because content
addressing makes the same append allocate the same ids anywhere. PR review
caught it; the case now also reads before the retry, so a fresh-store
regression fails instead of passing.)
`TestSQLiteTraceStoreConformance` is the SQLite fixture; `store_test.go`
keeps its per-backend tests, which mirrors Python, where this suite was
lifted *from* `test_trace_store.py`.

Full mapping, Python case → Go subtest under `TestSQLiteTraceStoreConformance`:

| Python test | Go subtest | Notes |
|---|---|---|
| test_append_then_read_owner_prefix | append_then_read_owner_prefix | |
| test_append_intent_idempotent_across_restart | append_intent_idempotent_across_restart | whole-receipt equality, as in Python (`second == first`): an idempotent retry returns the persisted receipt verbatim (`receiptFromJSON` of the stored `receipt_json`), so commit receipts, owner ranges, causal edges and context receipts all compare; the case also reads after reopen and *before* the retry, which pins that the factory reopened the same durable store |
| test_same_intent_different_batch_is_rejected | same_intent_different_batch_is_rejected | payload `{"value": 1}` asserted as `json.Number("1")` — retained bodies keep JSON integers as numbers |
| test_preview_record_ids_match_append | preview_record_ids_match_append | `preview_fact_ids` folded in: the Go store exposes one preview method |
| test_fact_id_is_content_addressed_across_intents | fact_id_is_content_addressed_across_intents | |
| test_content_addressed_fact_spans_multiple_owner_paths | content_addressed_fact_spans_multiple_owner_paths | owner asserted via `Record.View.TraceOwnerID` |
| test_cut_publish_resolve_roundtrip | cut_publish_resolve_roundtrip | |
| test_read_owner_cutoff_roundtrips_a_published_cut | read_owner_cutoff_roundtrips_a_published_cut | |
| test_causal_closure_includes_parents | causal_closure_includes_parents | Go's `ReadCausalClosure` takes an explicit closure policy; the case passes `include_external_anchors` |
| test_causal_parent_must_exist | causal_parent_must_exist | Python pins the `TraceStoreError` base class; the Go error vocabulary has no shared base, so the case pins the concrete `UnknownFactError` |
| test_descriptor_projection_resolution_roundtrip | N/A | run-output descriptor group: exercises `shepherd2.schemas.run_outputs`, deliberately unported (PARITY-PLAN resolution table); lands as a minimal subset with plan 04 (settlement) |
| test_descriptor_resolution_rejects_fact_outside_frontier_or_owner_path | N/A | same — re-evaluate at plan 04; visibility-stability itself is separately pinned by the frontier/cut immutability laws |
| test_descriptor_resolution_rejects_output_name_mismatch | N/A | same |
| test_descriptor_resolution_rejects_malformed_citation | N/A | same |
| test_descriptor_projection_rejects_duplicate_output_names | N/A | same |
| test_descriptor_resolution_rejects_frontier_and_owner_mismatch | N/A | same |
| test_descriptor_resolution_rejects_duplicate_output_names | N/A | same |
| test_descriptor_resolution_through_store_wrapper | N/A | same |

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

### ✅ §6 CLOSED: `docs/law-coverage.md` + `laws_test.go`

The coverage map lives at `docs/law-coverage.md` (the plan's alternative
`testdata/law-coverage.md` location was not taken: the map is documentation,
not test data, and the master plan's T1.8 gates on `docs/`). Twenty laws gained
dedicated tests in `laws_test.go`; laws already pinned by earlier tests keep
their homes, mapped in the doc.

Porting the laws — not inspecting the code — surfaced three genuine divergences,
all fixed in `store.go` in the same change:

1. **Duplicate causal parents were deduplicated, not rejected.** `resolvedCauses`
   silently deduplicated a parent cited at both the group and the draft level;
   Python rejects the append (`ValueError("duplicate causal parent")`). The
   dedupe changed the parent tuple a record digests over, so the same caller
   input could produce a different record id depending on which level the
   caller used — an identity divergence the digest vectors could not see,
   because no vector cites a parent twice.
2. **Witness bodies were not validated.** `ordinaryWitnessPlan` digested the
   witness body without validating it, so a group context with an empty
   `substrate_ref` or an unknown containment was retained — on append and on
   preview — as a witness whose body the kernel's own schema rejects.
   `ValidateWitnessBody` now runs before the digest, in the same place Python
   validates (`_ordinary_witness_plan`).
3. **Closure read order was randomized.** `canonicalFactOrder` sorted its rows
   in SQL (`ORDER BY path_ref, path_ordinal, record_id`) and then discarded
   the sort by ranging over a Go map, so every `ReadCausalClosure` result was
   in a different order on each process run — a single run always looked
   correct. Law 19's two-fact order assertion failed on roughly every fourth
   `go test` invocation before the fix. Same defect class as §2b: a Go
   structure backed by a map and compared against Python is suspect.

Of the anticipated gaps, all but one was real: duplicate-parent rejection,
local-ref pre-retention, witness-cycle rejection, support-closure
dedupe/mode-independence, closure-policy anchor behavior, mode-filter
non-pruning and full_internal authority all needed tests (and two needed the
fixes above). The exception is *operation-context non-cross-authorization for
cut publication* — covered by construction, because Go's `AppendContext`
carries no operation; each store method fixes its own `OperationKind`, so the
caller cannot present an append context to `PublishCut`. The Python law guards
an API shape the Go port does not expose.

The two accepted gaps, recorded in the map: law 21's projection half and law
23, both plan-02 dependencies (`ProjectionSpec`/`ensure_projection_compatible`
do not exist yet). Each flips to ✅ in the change that lands the schema library.

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

- [x] Payloads containing `<`, `>`, `&`, floats (`0.0`, `-0.0`, `1e16`, `1e-7`)
      produce digests identical to Python `canonical_digest` (Python-generated
      vectors in-tree).
      *(Done — `canonical_edge_vectors_v0.json` pins these by name and
      `canonical_corpus_v0.json` adds 200 seeded payloads including subnormals
      and both notation boundaries; every digest reproduces byte-for-byte.)*
- [x] Shared golden file hash matches the Python repo's copy; CI fails on drift.
      *(Done — the pinned-hash test fails the build on drift; the upstream
      compare is a deliberate local check, see the T1.4 row in plan 00.)*
- [x] `store_vectors_v0.json` replay passes: all record/context/frontier IDs
      byte-identical to Python store output.
      *(Done — §4 is closed: the store allocates the reference identities,
      replayed ID-for-ID.)*
- [x] Every Python conformance case either ported or documented N/A.
      *(Done — §5 mapping table: 10 ported into `conformance_test.go`'s
      `runConformance`, 8 run-output descriptor cases documented N/A pending
      plan 04.)*
- [x] 25-law coverage table complete; all laws pass or have documented
      deliberate-divergence rationale. *(Done — `docs/law-coverage.md`: 22 laws
      pass, law 21 is half-covered with its projection half documented as a
      plan-02 dependency, law 23 likewise, and law 24 is covered by construction
      with rationale. Three porting-found store fixes — `resolvedCauses`,
      `ordinaryWitnessPlan`, `canonicalFactOrder` — landed with the tests.)*
- [x] `ReadPathPrefix` implemented + tested; `AnchorKind` populated.
      *(Done — `d039f0c`, a documented alias; `AnchorKind` was already
      populated by the Phase 0 bug batch, §2a.)*
