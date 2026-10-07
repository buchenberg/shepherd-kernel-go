#!/usr/bin/env python3
"""Generate testdata/execution_vectors_v0.json from the Python reference.

Run from the repository root:

    python testdata/generate_execution_vectors.py testdata/execution_vectors_v0.json

T2a.6 vectors for plan 02: the execution/relation identity derivations and
the full record sequence a runtime run produces, taken from the authoritative
implementation so the Go port is measured against it, not against a guess.

Groups:
- execution_ids / relation_ids: execution_id_for / relation_id_for over
  several (append_intent_id, local_ref) pairs, including the defaults and a
  non-ASCII intent. These are plain sha256 over "<intent>\\0<local_ref>" --
  NOT canonical JSON -- truncated to 32 hex with an exec:/rel: prefix; the
  vectors pin that so nobody "fixes" it into canonical form.
- published_fact: the exact draft shape TaskControl.publish appends
  (schema shepherd2.runtime.published_fact.v1, kind fact_published, capture).
- run_sequence: a real @task run through runtime/handles.py -- create,
  one published fact, complete, terminal frontier -- recorded as ordered
  steps (intent, caused_by, allocated fact ids) plus the terminal cutoff,
  so the Go side can replay the identical steps and must allocate identical
  ids. The task_ref the runtime derives ("__main__.VectorTask") is recorded;
  the Go replay passes the same string rather than deriving it.
- fail_sequence: create -> fail -> frontier driven through the same batch
  builders, pinning the failed path's identities.

The Python checkout defaults to the sibling `../shepherd` directory; the
import path is the single source of truth for provenance (see
generate_canonical_corpus.py for the reasoning).

Do not edit the generated JSON by hand. Regenerating changes its hash, so
update the pinned SHA-256 in golden_provenance_test.go in the same commit.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

DEFAULT_SHEPHERD_REPO = str(Path(__file__).resolve().parent.parent.parent / "shepherd")

REPO_ENV = os.environ.get("SHEPHERD_REPO")
SRC_ENV = os.environ.get("SHEPHERD2_SRC")
SRC_IS_DEFAULT = SRC_ENV is None

if SRC_ENV is None:
    SRC_ENV = str(Path(REPO_ENV or DEFAULT_SHEPHERD_REPO) / "shepherd2" / "src")
SRC = SRC_ENV
sys.path.insert(0, SRC)

from shepherd2.kernel.facts import (  # noqa: E402
    TRUSTED_APPEND_CONTEXT,
    TRUSTED_READ_CONTEXT,
    Fact,
)
from shepherd2.runtime.handles import task  # noqa: E402
from shepherd2.schemas.execution import (  # noqa: E402
    complete_execution_batch,
    create_execution_batch,
    execution_id_for,
    fail_execution_batch,
    publish_execution_frontier,
)
from shepherd2.schemas.history import project_effective_history_from_store  # noqa: E402
from shepherd2.schemas.relations import (  # noqa: E402
    create_execution_relation_batch,
    relation_id_for,
)
from shepherd2.trace_store import SQLiteTraceStore  # noqa: E402

import subprocess  # noqa: E402


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


EXECUTION_ID_CASES = [
    ("vector:run:one:create", "execution"),
    ("vector:run:one:child:1:create", "execution"),
    ("vector:run:two:create", "execution"),
    ("intent:with-unicode:ün:create", "execution"),
    ("vector:run:one:create", "custom-local-ref"),
]

RELATION_ID_CASES = [
    ("vector:run:one:child:1:relation:spawned", "relation"),
    ("vector:run:one:relation:adopted:1", "relation"),
    ("vector:run:one:relation:abandoned:1", "relation"),
    ("vector:run:one:child:1:relation:spawned", "custom-local-ref"),
]

RUN_ID = "vector:run:one"
FAIL_RUN_ID = "vector:run:fail"
PARENT_RUN_ID = "vector:parent"
CHILD_RUN_ID = "vector:parent:child:1"
TREE_RUN_ID = "vector:tree"

PUBLISH_SCHEMA = "shepherd2.runtime.published_fact.v1"


@task
class VectorTask:
    def __init__(self, answer=0, label=""):
        self.answer = answer
        self.label = label

    def execute(self, control):
        control.publish("note", {"msg": "hello <>&", "n": 1.5})
        return {"answer": self.answer, "label": self.label}


def cutoff_payload(cutoff) -> dict:
    return {
        "frontier_id": cutoff.frontier_id,
        "target_trace_owner_id": cutoff.target_trace_owner_id,
        "through_fact_id": cutoff.through_fact_id,
        "through_owner_ordinal": cutoff.through_owner_ordinal,
        "publisher_trace_owner_id": cutoff.publisher_trace_owner_id,
        "created_by_fact_id": cutoff.created_by_fact_id,
    }


def owner_path_facts(store, execution_id) -> list:
    """The retained owner path with each fact's schema and mode."""
    trace_slice = store.read_owner_prefix(TRUSTED_READ_CONTEXT, execution_id, 99)
    facts = []
    for fact_id in trace_slice.owner_paths.get(execution_id, ()):
        fact = trace_slice.visible_facts_by_id.get(fact_id)
        if not isinstance(fact, Fact):
            raise TypeError("vector generation requires payload-visible facts")
        facts.append(
            {
                "fact_id": fact.envelope.fact_id,
                "schema_ref": fact.envelope.schema_ref,
                "mode": fact.envelope.mode,
                "kind_label": fact.view.kind_label if fact.view else "",
            }
        )
    return facts


def build_run_sequence(store) -> dict:
    """A real @task run, recorded step by step."""
    run = VectorTask.start(store=store, run_id=RUN_ID, answer=42, label="ok")
    cutoff = run.cutoff
    started = run.execution_id  # noqa: F841 - readability
    return {
        "run_id": RUN_ID,
        "execution_id": run.execution_id,
        "frontier_id": run.frontier_id,
        "task_ref": "__main__.VectorTask",
        "inputs": {"answer": 42, "label": "ok"},
        "steps": [
            {
                "kind": "create",
                "append_intent_id": f"{RUN_ID}:create",
                "caused_by": [],
            },
            {
                "kind": "publish",
                "append_intent_id": f"{RUN_ID}:publish:1",
                "schema_ref": PUBLISH_SCHEMA,
                "kind_label": "fact_published",
                "payload": {"kind": "note", "data": {"msg": "hello <>&", "n": 1.5}},
                "caused_by": "@prev:created+started",
            },
            {
                "kind": "complete",
                "append_intent_id": f"{RUN_ID}:complete",
                "outputs": {"answer": 42, "label": "ok"},
                "caused_by": "@prev:published",
            },
            {
                "kind": "frontier",
                "frontier_id": f"frontier:{RUN_ID}:terminal",
                "through": "@terminal",
                "publisher": None,
                "caused_by": [],
            },
        ],
        "owner_path": owner_path_facts(store, run.execution_id),
        "cutoff": cutoff_payload(cutoff),
    }


def build_fail_sequence(store) -> dict:
    """Create -> fail -> frontier through the batch builders, ids recorded."""
    execution_id = execution_id_for(f"{FAIL_RUN_ID}:create")
    create = store.append(
        TRUSTED_APPEND_CONTEXT,
        create_execution_batch(
            append_intent_id=f"{FAIL_RUN_ID}:create",
            execution_id=execution_id,
            task_ref="VectorTask",
            inputs={"boom": True},
            parent_execution_id=None,
        ),
    )
    fail = store.append(
        TRUSTED_APPEND_CONTEXT,
        fail_execution_batch(
            append_intent_id=f"{FAIL_RUN_ID}:fail",
            execution_id=execution_id,
            error="ValueError: boom",
            caused_by=(create.fact_ids[-1],),
        ),
    )
    frontier = publish_execution_frontier(
        store,
        TRUSTED_APPEND_CONTEXT,
        frontier_id=f"frontier:{FAIL_RUN_ID}:terminal",
        target_execution_id=execution_id,
        through_fact_id=fail.fact_ids[-1],
    )
    return {
        "run_id": FAIL_RUN_ID,
        "execution_id": execution_id,
        "frontier_id": f"frontier:{FAIL_RUN_ID}:terminal",
        "task_ref": "VectorTask",
        "inputs": {"boom": True},
        "steps": [
            {
                "kind": "create",
                "append_intent_id": f"{FAIL_RUN_ID}:create",
                "caused_by": [],
                "fact_ids": list(create.fact_ids),
            },
            {
                "kind": "fail",
                "append_intent_id": f"{FAIL_RUN_ID}:fail",
                "error": "ValueError: boom",
                "caused_by": [create.fact_ids[-1]],
                "fact_ids": list(fail.fact_ids),
            },
            {
                "kind": "frontier",
                "frontier_id": f"frontier:{FAIL_RUN_ID}:terminal",
                "through_fact_id": fail.fact_ids[-1],
                "publisher": None,
                "caused_by": [],
            },
        ],
        "owner_path": owner_path_facts(store, execution_id),
        "cutoff": cutoff_payload(frontier),
    }


def build_relation_sequence(store) -> dict:
    """Parent execution, child execution, and a spawned relation between
    them, recorded so the Go replay must allocate identical ids."""
    parent_execution_id = execution_id_for(f"{PARENT_RUN_ID}:create")
    child_execution_id = execution_id_for(f"{CHILD_RUN_ID}:create")
    child_frontier_id = f"frontier:{CHILD_RUN_ID}:terminal"

    parent_create = store.append(
        TRUSTED_APPEND_CONTEXT,
        create_execution_batch(
            append_intent_id=f"{PARENT_RUN_ID}:create",
            execution_id=parent_execution_id,
            task_ref="ParentTask",
            inputs={},
        ),
    )
    child_create = store.append(
        TRUSTED_APPEND_CONTEXT,
        create_execution_batch(
            append_intent_id=f"{CHILD_RUN_ID}:create",
            execution_id=child_execution_id,
            task_ref="ChildTask",
            inputs={"which": "first"},
            parent_execution_id=parent_execution_id,
            caused_by=(parent_create.fact_ids[-1],),
        ),
    )
    child_complete = store.append(
        TRUSTED_APPEND_CONTEXT,
        complete_execution_batch(
            append_intent_id=f"{CHILD_RUN_ID}:complete",
            execution_id=child_execution_id,
            outputs={"order": "child"},
            caused_by=(child_create.fact_ids[-1],),
        ),
    )
    publish_execution_frontier(
        store,
        TRUSTED_APPEND_CONTEXT,
        frontier_id=child_frontier_id,
        target_execution_id=child_execution_id,
        through_fact_id=child_complete.fact_ids[-1],
    )
    relation_intent = f"{CHILD_RUN_ID}:relation:spawned"
    relation_id = relation_id_for(relation_intent)
    relation = store.append(
        TRUSTED_APPEND_CONTEXT,
        create_execution_relation_batch(
            append_intent_id=relation_intent,
            relation_id=relation_id,
            relation_kind="spawned",
            parent_execution_id=parent_execution_id,
            child_execution_id=child_execution_id,
            child_frontier_id=child_frontier_id,
            caused_by=(parent_create.fact_ids[-1],),
        ),
    )
    return {
        "parent_run_id": PARENT_RUN_ID,
        "child_run_id": CHILD_RUN_ID,
        "parent_execution_id": parent_execution_id,
        "child_execution_id": child_execution_id,
        "child_frontier_id": child_frontier_id,
        "relation_intent": relation_intent,
        "relation_id": relation_id,
        "parent_create_intent": f"{PARENT_RUN_ID}:create",
        "parent_task_ref": "ParentTask",
        "child_create_intent": f"{CHILD_RUN_ID}:create",
        "child_task_ref": "ChildTask",
        "child_inputs": {"which": "first"},
        "child_complete_intent": f"{CHILD_RUN_ID}:complete",
        "child_outputs": {"order": "child"},
        "relation_fact_id": relation.fact_ids[0],
    }



@task
class TreeChildTask:
    def execute(self):
        return {"leaf": True}

@task
class TreeParentTask:
    def execute(self, control):
        control.publish("phase", {"at": "start"})
        kept = control.spawn(TreeChildTask)
        dropped = control.spawn(TreeChildTask)
        control.abandon(dropped)
        external = TreeChildTask.start(store=control.store, run_id=f"{TREE_RUN_ID}:external")
        control.adopt(execution_id=external.execution_id, frontier_id=external.frontier_id)
        control.publish("phase", {"at": "end"})
        return {"leaves": 2}

def build_history_sequence(store) -> dict:
    """A run tree through the real Python handles -- spawn, spawn-then-abandon,
    adopt of an externally created execution, two published facts -- with the
    effective history the reference projects from the run frontier."""
    run = TreeParentTask.start(store=store, run_id=TREE_RUN_ID)
    history = project_effective_history_from_store(store, TRUSTED_READ_CONTEXT, run.cutoff)
    return {
        "run_id": TREE_RUN_ID,
        "execution_id": run.execution_id,
        "frontier_id": run.frontier_id,
        "task_refs": {"parent": "__main__.TreeParentTask", "child": "__main__.TreeChildTask"},
        "parent_owner_path": owner_path_facts(store, run.execution_id),
        "root_status": history.root.status,
        "root_task_ref": history.root.task_ref,
        "children": [
            {
                "execution_id": child.execution.execution_id,
                "task_ref": child.execution.task_ref,
                "status": child.execution.status,
                "kind": child.relation.relation_kind,
                "relation_id": child.relation.relation_id,
                "frontier_id": child.relation.child_frontier_id,
            }
            for child in history.children
        ],
        "published": [fact.kind for fact in history.published_facts],
    }

def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2

    doc = {
        "generator": "testdata/generate_execution_vectors.py",
        "source_repo": "../shepherd" if (REPO_ENV is None and SRC_IS_DEFAULT) else repo_root(SRC),
        "source_commit": git_commit(SRC),
        "python_version": sys.version.split()[0],
        "execution_ids": [
            {
                "append_intent_id": intent,
                "local_ref": local_ref,
                "execution_id": execution_id_for(intent, local_ref),
            }
            for intent, local_ref in EXECUTION_ID_CASES
        ],
        "relation_ids": [
            {
                "append_intent_id": intent,
                "local_ref": local_ref,
                "relation_id": relation_id_for(intent, local_ref),
            }
            for intent, local_ref in RELATION_ID_CASES
        ],
        "published_fact": {
            "schema_ref": PUBLISH_SCHEMA,
            "kind_label": "fact_published",
            "mode": "capture",
            "payload": {"kind": "note", "data": {"msg": "hello <>&", "n": 1.5}},
        },
        "run_sequence": build_run_sequence(SQLiteTraceStore()),
        "fail_sequence": build_fail_sequence(SQLiteTraceStore()),
        "relation_sequence": build_relation_sequence(SQLiteTraceStore()),
        "history_sequence": build_history_sequence(SQLiteTraceStore()),
    }

    dest = sys.argv[1]
    with open(dest, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(doc, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")

    print(f"wrote {dest}")
    print(f"  source_commit: {doc['source_commit'][:12]}")
    print(
        f"  execution_ids={len(doc['execution_ids'])} relation_ids={len(doc['relation_ids'])}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
