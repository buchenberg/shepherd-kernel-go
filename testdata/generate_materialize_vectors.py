#!/usr/bin/env python3
"""Generate testdata/materialize_vectors_v0.json from the Python reference.

Run from the repository root:

    python testdata/generate_materialize_vectors.py testdata/materialize_vectors_v0.json

T2b.5 vectors for plan 03: the request digests and the record identities a
materialize transition produces, taken from the authoritative implementation
so the Go port is measured against it, not against a guess.

Groups:
- request_digests: _request_digest over several MaterializationRequest +
  OperationContext pairs, including the default cutoff and an explicit
  capture owner. The digest is an internal idempotency token, but pinning it
  pins the encoder flavour too (CPython json.dumps defaults, the same one
  the store uses for context ids), which is load-bearing for ledger
  cross-readability.
- echo_sequence: declare -> materialize through the reference EchoSubstrate,
  recording the declaration fact ids, the receipt, and the full capture
  record (envelope, payload, witness body) so the Go replay must allocate
  identical ids and retain identical content.
- kv_sequence: the same through SQLiteKVSubstrate with a temp world-side
  database, pinning the kv anchors and the applied capture.
- replay_sequence: declare -> materialize -> materialize again after a store
  restart, pinning that the ledger replays the stored receipt without
  redispatching the substrate (the counting substrate's call count is
  recorded).

The Python checkout defaults to the sibling `../shepherd` directory; the
import path is the single source of truth for provenance (see
generate_canonical_corpus.py for the reasoning).

Do not edit the generated JSON by hand. Regenerating changes its hash, so
update the pinned SHA-256 in golden_provenance_test.go in the same commit.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

DEFAULT_SHEPHERD_REPO = str(Path(__file__).resolve().parent.parent.parent / "shepherd")

REPO_ENV = os.environ.get("SHEPHERD_REPO")
SRC_ENV = os.environ.get("SHEPHERD2_SRC")
SRC_IS_DEFAULT = SRC_ENV is None

if SRC_ENV is None:
    SRC_ENV = str(Path(REPO_ENV or DEFAULT_SHEPHERD_REPO) / "shepherd2" / "src")
SRC = SRC_ENV
sys.path.insert(0, SRC)

from shepherd2 import (  # noqa: E402
    AppendBatch,
    AppendContext,
    AppendGroup,
    EchoSubstrate,
    FactDraft,
    MaterializationRequest,
    OperationContext,
    RetainedContextDraft,
    SQLiteKVSubstrate,
    SQLiteTraceStore,
    SubstrateRegistry,
    materialize,
)
from shepherd2.kernel.facts import Fact, ReadContext  # noqa: E402
from shepherd2.vnext.materialization import (  # noqa: E402
    MAX_OWNER_ORDINAL,
    _request_digest,
)


def repo_root(src: str) -> str:
    try:
        return (
            subprocess.check_output(
                ["git", "-C", src, "rev-parse", "--show-toplevel"],
                stderr=subprocess.DEVNULL,
            )
            .decode()
            .strip()
        )
    except Exception:  # pragma: no cover - provenance only
        return str(Path(src).parent.parent)


def git_commit(path):
    try:
        return (
            subprocess.check_output(
                ["git", "-C", path, "rev-parse", "HEAD"], stderr=subprocess.DEVNULL
            )
            .decode()
            .strip()
        )
    except Exception as exc:  # pragma: no cover - provenance only
        return f"unknown ({exc})"


APPEND = AppendContext(
    actor_ref="runtime:vector",
    presented_witness_refs=("trusted:internal",),
    schema_version_set="shepherd2-slice-a",
    trust_mode="internal",
)
MATERIALIZE = OperationContext(
    actor_ref="runtime:vector",
    operation="materialize",
    presented_authority_refs=("trusted:internal",),
    schema_environment_ref="shepherd2-slice-a",
    trust_mode="internal",
)
READ = ReadContext(actor_ref="vnext:materialize")


def _record_json(record) -> dict:
    assert isinstance(record, Fact), f"expected a payload-visible Fact, got {type(record)}"
    return {
        "record_id": record.envelope.record_id,
        "digest": record.envelope.digest,
        "schema_ref": record.envelope.schema_ref,
        "mode": record.envelope.mode,
        "witness_ref": record.envelope.witness_ref,
        "caused_by": list(record.envelope.caused_by_record_ids),
        "kind_label": record.fact_kind,
        "payload": record.body.payload,
    }


def _witness_payload(store, witness_ref) -> dict:
    witness = store.read_fact(READ, witness_ref)
    assert isinstance(witness, Fact), f"witness {witness_ref} not payload-visible"
    return {
        "record_id": witness.envelope.record_id,
        "substrate_ref": witness.body.payload["substrate_ref"],
        "containment": witness.body.payload["containment"],
    }


def _receipt_json(receipt) -> dict:
    return {
        "outcome": receipt.outcome,
        "substrate_ref": receipt.substrate_ref,
        "target_record_ids": list(receipt.target_record_ids),
        "produced_record_ids": list(receipt.produced_record_ids),
        "failure_reason": receipt.failure_reason,
        "world_side_anchors": [dict(a) for a in receipt.world_side_anchors],
    }


def _echo_sequence() -> dict:
    store = SQLiteTraceStore()
    registry = SubstrateRegistry()
    registry.register(EchoSubstrate("vector.echo.v1", frozenset({"vector.write.v1"})))
    declaration = store.append(
        APPEND,
        AppendBatch(
            append_intent_id="vector:echo:declare",
            groups=(
                AppendGroup(
                    trace_owner_id="owner:vector-echo",
                    retained_context=RetainedContextDraft(
                        substrate_ref="vector.echo.v1", containment="contained"
                    ),
                    fact_drafts=(
                        FactDraft(
                            mode="declaration",
                            schema_ref="vector.write.v1",
                            kind_label="write",
                            payload={"path": "note.txt", "text": "hello <>&", "n": 42},
                        ),
                    ),
                ),
            ),
        ),
    )
    request = MaterializationRequest(
        append_intent_id="vector:echo:apply",
        target_trace_owner_id="owner:vector-echo",
        target_record_ids=declaration.fact_ids,
    )
    receipt = materialize(store, MATERIALIZE, request, registry)
    capture = store.read_fact(READ, receipt.produced_record_ids[0])
    return {
        "declaration": {
            "intent": "vector:echo:declare",
            "owner": "owner:vector-echo",
            "substrate_ref": "vector.echo.v1",
            "fact_ids": list(declaration.fact_ids),
            "drafts": [
                {
                    "mode": "declaration",
                    "schema_ref": "vector.write.v1",
                    "kind_label": "write",
                    "payload": {"path": "note.txt", "text": "hello <>&", "n": 42},
                }
            ],
        },
        "request": {
            "append_intent_id": "vector:echo:apply",
            "target_trace_owner_id": "owner:vector-echo",
            "target_through_owner_ordinal": MAX_OWNER_ORDINAL,
            "capture_trace_owner_id": None,
        },
        "receipt": _receipt_json(receipt),
        "capture": _record_json(capture),
        "witness": _witness_payload(store, capture.envelope.witness_ref),
    }


def _kv_sequence() -> dict:
    store = SQLiteTraceStore()
    with tempfile.TemporaryDirectory() as tmp:
        kv_path = Path(tmp) / "world.sqlite"
        with SQLiteKVSubstrate(kv_path) as substrate:
            registry = SubstrateRegistry()
            registry.register(substrate)
            declaration = store.append(
                APPEND,
                AppendBatch(
                    append_intent_id="vector:kv:declare",
                    groups=(
                        AppendGroup(
                            trace_owner_id="owner:vector-kv",
                            retained_context=RetainedContextDraft(
                                substrate_ref=substrate.substrate_ref
                            ),
                            fact_drafts=(
                                FactDraft(
                                    mode="declaration",
                                    schema_ref="shepherd2.kv.put.v1",
                                    kind_label="kv_put",
                                    payload={"key": "answer", "value": {"n": 42}},
                                ),
                            ),
                        ),
                    ),
                ),
            )
            request = MaterializationRequest(
                append_intent_id="vector:kv:apply",
                target_trace_owner_id="owner:vector-kv",
                target_record_ids=declaration.fact_ids,
            )
            receipt = materialize(store, MATERIALIZE, request, registry)
            capture = store.read_fact(READ, receipt.produced_record_ids[0])
            kv_value = substrate.get("answer")
            return {
                "declaration": {
                    "intent": "vector:kv:declare",
                    "owner": "owner:vector-kv",
                    "substrate_ref": substrate.substrate_ref,
                    "fact_ids": list(declaration.fact_ids),
                    "drafts": [
                        {
                            "mode": "declaration",
                            "schema_ref": "shepherd2.kv.put.v1",
                            "kind_label": "kv_put",
                            "payload": {"key": "answer", "value": {"n": 42}},
                        }
                    ],
                },
                "request": {
                    "append_intent_id": "vector:kv:apply",
                    "target_trace_owner_id": "owner:vector-kv",
                    "target_through_owner_ordinal": MAX_OWNER_ORDINAL,
                    "capture_trace_owner_id": None,
                },
                "receipt": _receipt_json(receipt),
                "capture": _record_json(capture),
                "witness": _witness_payload(store, capture.envelope.witness_ref),
                "world_value": kv_value,
            }


class _CountingSubstrate:
    substrate_ref = "vector.count.v1"
    declaration_schemas = frozenset({"vector.write.v1"})
    capture_schemas = frozenset({"vector.write.applied.v1"})
    containment = "contained"

    def __init__(self) -> None:
        self.calls = 0

    def materialize(self, records):
        self.calls += 1
        return _counting_result(self.calls, records)


def _counting_result(calls, records):
    from shepherd2 import FactDraft, MaterializationResult

    return MaterializationResult(
        outcome="success",
        capture_drafts=(
            FactDraft(
                mode="capture",
                schema_ref="vector.write.applied.v1",
                kind_label="write_applied",
                payload={"call": calls},
                caused_by_fact_ids=(records[0].envelope.record_id,),
            ),
        ),
    )


def _replay_sequence() -> dict:
    with tempfile.TemporaryDirectory() as tmp:
        db_path = Path(tmp) / "trace.sqlite"
        substrate = _CountingSubstrate()
        registry = SubstrateRegistry()
        registry.register(substrate)
        store = SQLiteTraceStore(db_path)
        declaration = store.append(
            APPEND,
            AppendBatch(
                append_intent_id="vector:replay:declare",
                groups=(
                    AppendGroup(
                        trace_owner_id="owner:vector-replay",
                        retained_context=RetainedContextDraft(
                            substrate_ref=substrate.substrate_ref
                        ),
                        fact_drafts=(
                            FactDraft(
                                mode="declaration",
                                schema_ref="vector.write.v1",
                                kind_label="write",
                                payload={"value": 1},
                            ),
                        ),
                    ),
                ),
            ),
        )
        request = MaterializationRequest(
            append_intent_id="vector:replay:apply",
            target_trace_owner_id="owner:vector-replay",
            target_record_ids=declaration.fact_ids,
        )
        first = materialize(store, MATERIALIZE, request, registry)
        store.close()

        restarted = SQLiteTraceStore(db_path)
        second = materialize(restarted, MATERIALIZE, request, registry)
        restarted.close()

        return {
            "declaration_fact_ids": list(declaration.fact_ids),
            "first_receipt": _receipt_json(first),
            "second_receipt": _receipt_json(second),
            "substrate_calls": substrate.calls,
        }


REQUEST_DIGEST_CASES = [
    {
        "append_intent_id": "vector:digest:one",
        "target_trace_owner_id": "owner:digest",
        "target_record_ids": ["sha256:aaaa", "sha256:bbbb"],
        "target_through_owner_ordinal": MAX_OWNER_ORDINAL,
        "capture_trace_owner_id": None,
        "actor_ref": "runtime:vector",
        "authority_refs": ["trusted:internal"],
        "schema_environment_ref": "shepherd2-slice-a",
        "trust_mode": "internal",
    },
    {
        "append_intent_id": "vector:digest:two",
        "target_trace_owner_id": "owner:digest",
        "target_record_ids": ["sha256:cccc"],
        "target_through_owner_ordinal": 5,
        "capture_trace_owner_id": "owner:captures",
        "actor_ref": "runtime:vector",
        "authority_refs": ["trusted:internal"],
        "schema_environment_ref": "shepherd2-slice-a",
        "trust_mode": "internal",
    },
    {
        "append_intent_id": "vector:digest:ünïcode",
        "target_trace_owner_id": "owner:dïgest",
        "target_record_ids": ["sha256:dddd"],
        "target_through_owner_ordinal": 0,
        "capture_trace_owner_id": None,
        "actor_ref": "runtime:vector <actor>",
        "authority_refs": [],
        "schema_environment_ref": "shepherd2-slice-a",
        "trust_mode": None,
    },
]


def _digest_case(case: dict) -> str:
    request = MaterializationRequest(
        append_intent_id=case["append_intent_id"],
        target_trace_owner_id=case["target_trace_owner_id"],
        target_record_ids=tuple(case["target_record_ids"]),
        target_through_owner_ordinal=case["target_through_owner_ordinal"],
        capture_trace_owner_id=case["capture_trace_owner_id"],
    )
    context = OperationContext(
        actor_ref=case["actor_ref"],
        operation="materialize",
        presented_authority_refs=tuple(case["authority_refs"]),
        schema_environment_ref=case["schema_environment_ref"],
        trust_mode=case["trust_mode"],
    )
    return _request_digest(request, context)


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2

    doc = {
        "generator": "testdata/generate_materialize_vectors.py",
        "source_repo": "../shepherd" if (REPO_ENV is None and SRC_IS_DEFAULT) else repo_root(SRC),
        "source_commit": git_commit(SRC),
        "python_version": sys.version.split()[0],
        "request_digests": [
            {"request": case, "digest": _digest_case(case)}
            for case in REQUEST_DIGEST_CASES
        ],
        "echo_sequence": _echo_sequence(),
        "kv_sequence": _kv_sequence(),
        "replay_sequence": _replay_sequence(),
    }

    dest = sys.argv[1]
    with open(dest, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(doc, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")

    print(f"wrote {dest}")
    print(f"  source_commit: {doc['source_commit'][:12]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
