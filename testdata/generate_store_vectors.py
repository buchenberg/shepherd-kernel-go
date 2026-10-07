#!/usr/bin/env python3
"""Generate testdata/store_vectors_v0.json from the Python reference store.

Run from the repository root:

    python testdata/generate_store_vectors.py testdata/store_vectors_v0.json

The Python checkout defaults to the sibling `../shepherd` directory and can be
overridden with SHEPHERD_REPO (or SHEPHERD2_SRC for the import path alone).

This drives shepherd2's own `SQLiteTraceStore` against a fresh database per
fixture and records every identity it allocates — record ids, witness refs,
commit receipts, context ids, owner ordinals, causal edges, frontier ids and owner
path contents. The Go side replays the same batches and must produce the same
identities.

Digest-layer agreement does not prove the *store* agrees: the store builds the
digest inputs, so a difference in the witness plan, the retained-context payload
or the context-id algorithm produces different ids from identical content. That is
what these vectors exist to catch, and it did — record, witness and context ids all
differed before this was fixed.

Note the encoding asymmetry this relies on: shepherd2 runs two JSON encoders in
the append path. Record and witness ids use canonical_json_bytes
(ensure_ascii=False), while the context id and batch digest use `_json_dumps`,
which omits ensure_ascii and so escapes non-ASCII. Fixtures therefore include
non-ASCII content on both sides of that split.

Do not edit the generated JSON by hand. Regenerating changes its hash, so update
the pinned value in golden_provenance_test.go in the same commit.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

DEFAULT_SHEPHERD_REPO = str(Path(__file__).resolve().parent.parent.parent / "shepherd")
DEFAULT_SHEPHERD2_SRC = str(Path(DEFAULT_SHEPHERD_REPO) / "shepherd2" / "src")

SRC = os.environ.get("SHEPHERD2_SRC", DEFAULT_SHEPHERD2_SRC)
REPO = os.environ.get("SHEPHERD_REPO", DEFAULT_SHEPHERD_REPO)
sys.path.insert(0, SRC)

from shepherd2.kernel.facts import (  # noqa: E402
    AppendBatch,
    AppendContext,
    AppendGroup,
    CutSpec,
    ReadContext,
    RecordDraft,
    RetainedContextDraft,
)
from shepherd2.trace_store import SQLiteTraceStore, _batch_digest  # noqa: E402

TRUSTED = dict(
    actor_ref="runtime:conformance",
    presented_witness_refs=["trusted:internal"],
    schema_version_set="shepherd2-slice-a",
    trust_mode="internal",
)
READER = dict(actor_ref="reader", visibility_profile="payload")


def draft(kind, payload, *, mode="capture", caused_by=(), appends=None, local=None):
    return {
        "mode": mode,
        "schema_ref": f"shepherd2.conformance.{kind}.v1",
        "payload": payload,
        "kind_label": kind,
        "append_local_id": local,
        "caused_by_fact_ids": list(caused_by),
        "caused_by_local_refs": list(appends or ()),
    }


def group(owner, drafts, *, retained_context=None, causal_parents=()):
    return {
        "trace_owner_id": owner,
        "retained_context": retained_context,
        "causal_parents": list(causal_parents),
        "fact_drafts": drafts,
    }


# ---------------------------------------------------------------------------
# Fixtures. Each is a list of batches replayed in order against one fresh store.
# A frontier spec may be attached to a batch index, because publishing one appends
# a record and therefore consumes a commit sequence number.
# ---------------------------------------------------------------------------

FIXTURES = [
    {
        "label": "root_witness_auto_plan",
        "note": "The root witness is created regardless, and the ordinary witness "
        "for a default retained context cites it.",
        "batches": [{"append_intent_id": "intent:a", "groups": [group("exec:one", [draft("step", {"value": 1})])]}],
    },
    {
        "label": "ordinary_witness_non_default_context",
        "note": "A retained context with capability_witness_refs, so the witness "
        "authority_refs come from the context rather than the append context.",
        "batches": [{
            "append_intent_id": "intent:ctx",
            "groups": [group(
                "exec:ctx",
                [draft("step", {"value": 2})],
                retained_context={
                    "active_binding_refs": ["binding:one"],
                    "capability_witness_refs": ["trusted:capability"],
                    "semantic_environment_refs": ["schema-set:extra"],
                    "visibility_policy_refs": ["visibility:payload"],
                    "substrate_ref": "sqlite.local.v1",
                    "containment": "contained",
                    "reuse_context_id": None,
                },
            )],
        }],
    },
    {
        "label": "retained_context_reuse_by_id",
        "note": "Second batch reuses the first batch's context id, so the context "
        "receipt repeats and no new context row is created.",
        "batches": [
            {"append_intent_id": "intent:reuse-1", "groups": [group(
                "exec:reuse", [draft("step", {"value": 3})],
                retained_context={
                    "active_binding_refs": ["binding:reuse"],
                    "capability_witness_refs": [],
                    "semantic_environment_refs": [],
                    "visibility_policy_refs": [],
                    "substrate_ref": "sqlite.local.v1",
                    "containment": "contained",
                    "reuse_context_id": None,
                },
            )]},
            {"append_intent_id": "intent:reuse-2", "groups": [group(
                "exec:reuse", [draft("step", {"value": 4})],
                retained_context={"reuse_of_batch": 0, "reuse_of_group": 0},
            )]},
        ],
    },
    {
        "label": "multi_group_atomic_batch",
        "note": "Two owners in one intent, so owner ordinals advance independently "
        "and both witnesses are deduped by record id where the contexts match.",
        "batches": [{
            "append_intent_id": "intent:multi",
            "groups": [
                group("owner:a", [draft("step", {"value": 5})]),
                group("owner:b", [draft("step", {"value": 6})]),
            ],
        }],
    },
    {
        "label": "local_ref_resolution",
        "note": "caused_by_local_refs is resolved to the retained parent id before "
        "hashing, and the local ref never reaches the envelope.",
        "batches": [{
            "append_intent_id": "intent:local",
            "groups": [group("exec:local", [
                draft("parent", {"value": 7}, local="p1"),
                draft("child", {"value": 8}, appends=["p1"]),
            ])],
        }],
    },
    {
        "label": "frontier_publish",
        "note": "Publishing a frontier appends a cutoff record of its own, so it "
        "consumes a commit sequence number and produces an extra record id.",
        "batches": [{"append_intent_id": "intent:frontier", "groups": [group(
            "exec:frontier", [draft("step", {"value": 9})])]}],
        "frontiers": [{"after_batch": 0, "frontier_id": "frontier:one",
                       "target_trace_owner_id": "exec:frontier",
                       "through_last_fact_of": 0}],
    },
    {
        "label": "non_ascii_payload",
        "note": "Non-ASCII in the payload and in a retained-context ref, which is "
        "where the two encoders diverge: the payload goes through the "
        "ensure_ascii=False path and the context id through ensure_ascii=True.",
        "batches": [{
            "append_intent_id": "intent:unicode",
            "groups": [group(
                "exec:unicode",
                [draft("step", {"text": "caf\u00e9 \u65e5\u672c\u8a9e \U0001f600", "html": "<a>&b"})],
                retained_context={
                    "active_binding_refs": ["binding:caf\u00e9"],
                    "capability_witness_refs": [],
                    "semantic_environment_refs": ["schema-set:\u00e9"],
                    "visibility_policy_refs": [],
                    "substrate_ref": "sqlite.local.v1",
                    "containment": "contained",
                    "reuse_context_id": None,
                },
            )],
        }],
    },
    {
        "label": "intent_replay",
        "note": "Replaying an identical intent returns the stored receipt and "
        "creates no new records.",
        "batches": [
            {"append_intent_id": "intent:replay", "groups": [group("exec:replay", [draft("step", {"value": 10})])]},
            {"append_intent_id": "intent:replay", "groups": [group("exec:replay", [draft("step", {"value": 10})])]},
        ],
    },
    {
        "label": "declaration_and_capture",
        "note": "Mode is part of the record digest, so the same payload in two "
        "modes yields different ids.",
        "batches": [{"append_intent_id": "intent:modes", "groups": [group("exec:modes", [
            draft("step", {"value": 11}, mode="capture"),
            draft("step", {"value": 11}, mode="declaration"),
        ])]}],
    },
]


def build_drafts(specs):
    """Build drafts, leaving append-local refs for the store to resolve.

    Resolving them here would be wrong: a local ref may point at a sibling draft in
    the same batch, which does not have a record id until the append allocates it.
    shepherd2 resolves them inside the transaction, and so must the Go replay.
    """
    return tuple(
        RecordDraft(
            mode=spec["mode"],
            schema_ref=spec["schema_ref"],
            payload=spec["payload"],
            kind_label=spec["kind_label"],
            append_local_id=spec["append_local_id"],
            caused_by_fact_ids=tuple(spec["caused_by_fact_ids"]),
            caused_by_local_refs=tuple(spec["caused_by_local_refs"]),
        )
        for spec in specs
    )


def run_fixture(fixture, store_factory):
    """Replay one fixture and return its recorded identities."""
    store = store_factory()
    ctx = AppendContext(**TRUSTED)
    reader = ReadContext(**READER)

    receipts = []
    frontier_ids = []
    context_ids_by_key = {}
    local_ids = {}

    for batch_index, batch_spec in enumerate(fixture["batches"]):
        groups = []
        for gspec in batch_spec["groups"]:
            rc = gspec["retained_context"]
            if rc is not None and "reuse_of_batch" in rc:
                # A reuse reference is passed as a bare context id, which is the
                # shape _context_draft_for_group treats as reuse.
                retained = context_ids_by_key[(rc["reuse_of_batch"], rc["reuse_of_group"])]
            elif rc is not None:
                retained = RetainedContextDraft(
                    active_binding_refs=tuple(rc["active_binding_refs"]),
                    capability_witness_refs=tuple(rc["capability_witness_refs"]),
                    semantic_environment_refs=tuple(rc["semantic_environment_refs"]),
                    visibility_policy_refs=tuple(rc["visibility_policy_refs"]),
                    substrate_ref=rc["substrate_ref"],
                    containment=rc["containment"],
                    reuse_context_id=rc.get("reuse_context_id"),
                )
            else:
                retained = None

            groups.append(AppendGroup(
                trace_owner_id=gspec["trace_owner_id"],
                retained_context=retained,
                causal_parents=tuple(gspec["causal_parents"]),
                fact_drafts=build_drafts(gspec["fact_drafts"]),
            ))

        batch = AppendBatch(
            append_intent_id=batch_spec["append_intent_id"],
            groups=tuple(groups),
        )
        receipt = store.append(ctx, batch)

        for gi, cid in enumerate(receipt.context_receipts):
            context_ids_by_key[(batch_index, gi)] = cid
        # And its record ids for later causal parents.
        for draft_spec, fid in zip(
            [d for g in batch_spec["groups"] for d in g["fact_drafts"]], receipt.fact_ids
        ):
            if draft_spec.get("append_local_id"):
                local_ids[draft_spec["append_local_id"]] = fid

        receipts.append({
            "append_intent_id": receipt.append_intent_id,
            "fact_ids": list(receipt.fact_ids),
            "commit_receipts": list(receipt.commit_receipts),
            "context_receipts": list(receipt.context_receipts),
            "owner_ordinal_ranges": {
                owner: [rng[0], rng[1]] for owner, rng in receipt.owner_ordinal_ranges.items()
            },
            "causal_edges": [[a, b] for a, b in receipt.causal_edges],
            # The reference batch digest, so the Go port's batch-digest payload shape
            # is checked against Python rather than only against itself. Without it
            # the intent_replay fixture would compare two digests both computed by
            # the implementation under test, and any error in the translated shape
            # would still pass.
            "batch_digest": _batch_digest(batch, ctx),
        })

        for fspec in fixture.get("frontiers", []):
            if fspec["after_batch"] != batch_index:
                continue
            cut = store.publish_frontier(ctx, CutSpec(
                frontier_id=fspec["frontier_id"],
                target_trace_owner_id=fspec["target_trace_owner_id"],
                through_fact_id=receipt.fact_ids[fspec["through_last_fact_of"]],
            ))
            frontier_ids.append(cut.frontier_id)

    # Owner-path contents across every owner the fixture touched.
    owners = sorted({g["trace_owner_id"] for b in fixture["batches"] for g in b["groups"]}
                    | {"kernel:witness"})
    owner_paths = {}
    for owner in owners:
        sl = store.read_owner_prefix(reader, owner, 9999)
        ids = list(sl.fact_ids())
        owner_paths[owner] = ids
        labels = {}
        for fid in ids:
            view = getattr(sl.facts_by_id[fid], "view", None)
            if view is not None:
                labels[fid] = view.kind_label
        if labels:
            owner_paths.setdefault("_kind_labels", {}).update(labels)

    store.close()
    return {
        "label": fixture["label"],
        "note": fixture["note"],
        "append_context": TRUSTED,
        "batches": fixture["batches"],
        "frontiers": fixture.get("frontiers", []),
        "expected": {
            "receipts": receipts,
            "frontier_ids": frontier_ids,
            "owner_paths": owner_paths,
        },
    }


def git_commit(path):
    try:
        return subprocess.check_output(
            ["git", "-C", path, "rev-parse", "HEAD"], stderr=subprocess.DEVNULL
        ).decode().strip()
    except Exception as exc:  # pragma: no cover - provenance only
        return f"unknown ({exc})"


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2

    # ignore_cleanup_errors: on Windows an unclosed SQLite handle makes the temp
    # directory undeletable, which would mask the real error.
    with tempfile.TemporaryDirectory(ignore_cleanup_errors=True) as tmp:
        counter = {"n": 0}

        def store_factory():
            counter["n"] += 1
            return SQLiteTraceStore(Path(tmp) / f"fixture{counter['n']}.sqlite")

        vectors = [run_fixture(f, store_factory) for f in FIXTURES]

    doc = {
        "canonical_version": "shepherd.kernel.canonical.v2",
        "generator": "testdata/generate_store_vectors.py",
        "source_repo": "../shepherd",
        "source_commit": git_commit(REPO),
        "python_version": sys.version.split()[0],
        "vectors": vectors,
    }

    dest = sys.argv[1]
    with open(dest, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(doc, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")

    print(f"wrote {dest}")
    print(f"  source_commit: {doc['source_commit'][:12]}")
    print(f"  fixtures: {len(vectors)}")
    for v in vectors:
        r = v["expected"]["receipts"]
        print(f"    {v['label']:42} batches={len(r)} facts={sum(len(x['fact_ids']) for x in r)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
