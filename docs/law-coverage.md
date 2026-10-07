# ABI law coverage map

**Python anchor**: `../shepherd/shepherd2/tests/test_kernel_abi_v0_laws.py` — 25
executable laws that pin the kernel ABI (`test_kernel_abi_v0.py` pins the digest
vectors; this file pins the behaviors around them).

**Go counterparts**: laws with no existing home live in `laws_test.go`, named
`TestLaw<PythonNameMinusTest_>`; laws already covered by earlier tests keep
their original home. Cross-language digest laws are additionally pinned by
`testdata/kernel_abi_v0.json` (golden) and `testdata/store_vectors_v0.json`
(store-level replay).

**Three laws required store fixes, found by porting rather than by inspection**:

1. *Duplicate causal parents* (law 7): Go's `resolvedCauses` silently
   deduplicated a parent cited at both the group and the draft level, where
   Python rejects the append. The dedupe changed the parent tuple a record
   digests over, so the same caller input could produce a different record id
   depending on which level the caller used. Fixed in `store.go`.
2. *Witness body validation* (law 5): Go's `ordinaryWitnessPlan` digested the
   witness body without validating it, so a group context with an empty
   `substrate_ref` or an unknown containment was retained as a witness whose
   body the kernel's own schema rejects — on append and on preview. Fixed in
   `store.go`; `ValidateWitnessBody` now runs before the digest.
3. *Closure read order* (law 19): `canonicalFactOrder` sorted its rows in SQL
   and then threw the order away by ranging over a Go map, so every
   `ReadCausalClosure` result was randomized per process — a single run always
   looked correct, which is why it survived until a law test asserted a
   two-fact order and failed on roughly every fourth `go test` invocation.
   Fixed in `store.go`; the entries now keep the query's ORDER BY order. This
   is the same defect class plan 01 §2b recorded for slice outputs: any Go
   structure backed by a map and compared against Python is suspect.

## The 25 laws

| # | Python law | Go test(s) | Status |
|---|---|---|---|
| 1 | test_kernel_abi_v0_marker_is_frozen | `TestKernelABIVersionsAreFrozen` (canonical_test.go) — constant plus golden match | ✅ |
| 2 | test_record_id_is_digest_and_excludes_path_receipts | `TestFactIDIsContentAddressedAcrossIntents`, `TestContentAddressedFactSpansMultipleOwnerPaths`, `TestSameIntentDifferentBatchIsRejected` (re-digest identity) — store_test.go | ✅ |
| 3 | test_record_digest_excludes_legacy_kind_label | `TestLawRecordDigestExcludesLegacyKindLabel` | ✅ |
| 4 | test_witness_records_are_retained_and_non_root_records_have_witnesses | `TestLawWitnessRecordsAreRetainedAndNonRootRecordsHaveWitnesses`; digest-level halves also pinned by `TestWitnessRecordIDsAreNotBodyDigests`, `TestOrdinaryWitnessCitesRootWitness`, `TestRootWitnessRecordIDMatchesGolden` (canonical_test.go) | ✅ |
| 5 | test_witness_body_validation_is_enforced_on_append_and_preview | `TestLawWitnessBodyValidationIsEnforcedOnAppendAndPreview` — **required the `ordinaryWitnessPlan` fix** | ✅ |
| 6 | test_non_root_record_with_empty_witness_ref_rejected | `TestLawNonRootRecordWithEmptyWitnessRefRejected` | ✅ |
| 7 | test_ordered_causality_rejects_duplicate_parents | `TestLawOrderedCausalityRejectsDuplicateParents` — **required the `resolvedCauses` fix** | ✅ |
| 8 | test_append_local_refs_are_resolved_before_retention | `TestLawAppendLocalRefsAreResolvedBeforeRetention`; the `local_ref_resolution` fixture in store_vectors_v0.json replays the same behavior ID-for-ID | ✅ |
| 9 | test_shape_only_hides_payloads_and_preserves_context_anchor | `TestLawShapeOnlyHidesPayloadsAndPreservesContextAnchor` | ✅ |
| 10 | test_slice_exposes_visible_witness_records_under_payload_visibility | `TestLawSliceExposesVisibleWitnessRecordsUnderPayloadVisibility` | ✅ |
| 11 | test_slice_witness_support_is_closed_to_root_under_shape_visibility | `TestLawSliceWitnessSupportIsClosedToRootUnderShapeVisibility` | ✅ |
| 12 | test_witness_support_rejects_witness_cycles | `TestLawWitnessSupportRejectsWitnessCycles` — the cycle is induced by direct SQL, exactly as in Python: this is a corruption-detection law, and no append path can produce the state | ✅ |
| 13 | test_shape_only_witness_anchor_preserves_witness_ref_shape | `TestLawShapeOnlyWitnessAnchorPreservesWitnessRefShape` | ✅ |
| 14 | test_witness_support_ignores_mode_filter_and_does_not_change_selected_graph | `TestLawWitnessSupportIgnoresModeFilterAndDoesNotChangeSelectedGraph` | ✅ |
| 15 | test_witness_support_is_deduped_by_record_id | `TestLawWitnessSupportIsDedupedByRecordID` | ✅ |
| 16 | test_missing_witness_support_fails_loudly | `TestLawMissingWitnessSupportFailsLoudly` — SQL-induced dangling ref, as in Python | ✅ |
| 17 | test_external_anchor_preserves_out_of_cut_parent | `TestLawExternalAnchorPreservesOutOfCutParent`; anchor kind also pinned by `TestExternalAnchorKindIsFact` | ✅ |
| 18 | test_causal_closure_policy_controls_filtered_parent_anchors | `TestLawCausalClosurePolicyControlsFilteredParentAnchors`. The final Python sub-case — calling `read_causal_closure` with no policy is a `TypeError` — is not portable: Go's signature requires the argument, so the compiler rejects the call | ✅ (sub-case by construction) |
| 19 | test_causal_closure_mode_filter_does_not_prune_traversal | `TestLawCausalClosureModeFilterDoesNotPruneTraversal` — **required the `canonicalFactOrder` fix** | ✅ |
| 20 | test_full_internal_reads_require_trusted_authority | `TestLawFullInternalReadsRequireTrustedAuthority` | ✅ |
| 21 | test_cut_slice_mode_and_projection_laws | Mode-filter half: `TestLawCutSliceModeLaws`. Projection half: **plan 02 dependency** — `ProjectionSpec`/`ensure_projection_compatible`/`project_execution_slice` do not exist in the Go port yet | 🔄 mode ✅, projection → plan 02 |
| 22 | test_later_records_do_not_change_published_cut | `TestFrontierIsImmutable` (store_test.go) — cut resolves to the pre-cut facts while the owner prefix has grown | ✅ |
| 23 | test_projection_fails_on_incompatible_mode_filter | **Plan 02 dependency** — the projection-compatibility API lands with the schema library (plan 02, T2a.4); re-evaluate there. The underlying mode-filter behavior the projection depends on is pinned by law 21's mode half | ⬜ plan 02 |
| 24 | test_operation_context_does_not_cross_authorize_cut_publication | Covered **by construction**: Go's `AppendContext` carries no operation — each store method fixes its own `OperationKind` via `ToOperationContext`, so a caller cannot present an append context to `PublishCut`. The Python law guards an API shape Go does not expose; `TestReservedOperationKindsAreUnreachable` pins the adjacent reserved-kind behavior | ✅ by construction |
| 25 | test_restart_preserves_records_cuts_and_paths | `TestLawRestartPreservesRecordsCutsAndPaths`; record-restart also pinned by `TestAppendReadAcrossRestart` and the conformance suite's restart case | ✅ |

## Maintenance rule

A change that touches `store.go`'s append, read, or frontier paths must keep
every law green — `go test -run TestLaw ./...` is the quick gate. New laws
added upstream get a row here and a test in `laws_test.go` in the same change;
the two plan-02 dependencies (laws 21 projection half, 23) are the only
accepted gaps, and each must flip to ✅ in the change that lands plan 02's
schema library.
