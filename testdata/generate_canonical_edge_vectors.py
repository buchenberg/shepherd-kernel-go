#!/usr/bin/env python3
"""Generate testdata/canonical_edge_vectors_v0.json from the Python reference.

Run from the repository root:

    python testdata/generate_canonical_edge_vectors.py testdata/canonical_edge_vectors_v0.json

It imports shepherd2's own `canonical_json_bytes` / `canonical_digest` rather than
reimplementing them, so the expected bytes come from the authoritative
implementation. Point SHEPHERD2_SRC at the checkout if it is not at the default
location below.

Do not edit the generated JSON by hand. If the vectors change, update the pinned
SHA-256 in golden_provenance_test.go in the same commit and say why.

These vectors exist because the digest layer's contract is cross-implementation
byte-identity, and both historical divergences in it — HTML escaping of <, > and &,
and CPython's float repr — were invisible to the shared golden file, whose payloads
happen to contain only small ints and plain ASCII.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

DEFAULT_SHEPHERD2_SRC = r"C:\Code\Personal\agentic\shepherd\shepherd2\src"
DEFAULT_SHEPHERD_REPO = r"C:\Code\Personal\agentic\shepherd"

SRC = os.environ.get("SHEPHERD2_SRC", DEFAULT_SHEPHERD2_SRC)
REPO = os.environ.get("SHEPHERD_REPO", DEFAULT_SHEPHERD_REPO)
sys.path.insert(0, SRC)

from shepherd2.kernel.canonical import (  # noqa: E402
    CANONICAL_VERSION,
    canonical_digest,
    canonical_json_bytes,
)

# Float payloads. Each is wrapped in a one-key mapping because
# canonical_json_bytes takes a mapping at the top level.
FLOAT_PAYLOADS = [
    ("zero", 0.0),
    ("negative_zero", -0.0),
    ("simple_fraction", 2.5),
    ("one", 1.0),
    ("hundred", 100.0),
    ("negative_one", -1.0),
    ("binary_rounding", 0.1 + 0.2),
    ("just_below_exp_threshold", 1e15),
    ("exp_threshold", 1e16),
    ("above_exp_threshold", 1e17),
    ("fixed_lower_bound", 1e-4),
    ("exp_lower_bound", 1e-5),
    ("small_exp", 1e-7),
    ("two_pow_53", 9007199254740993.0),
    ("exp_with_fraction", 1.5e16),
    ("huge", 1e300),
    ("denormal_min", 5e-324),
    ("four_significant", 1234.5),
    ("twenty_one_exp", 1e21),
    ("twenty_two_exp", 1e22),
]

STRING_PAYLOADS = [
    ("html_script", "<script>a&&b</script>"),
    ("gt_only", "a>b"),
    ("quotes_backslash_slash", 'quote"backslash\\slash/'),
    ("control_short_forms", "tab\tnewline\ncr\rbell\x07"),
    ("null_char", "null\x00char"),
    ("non_ascii_bmp", "unicod\u00e9 \u00fcn\u00efc\u00f6d\u00e9 \u65e5\u672c\u8a9e"),
    ("non_bmp_emoji", "emoji \U0001f600"),
    ("ansi_escape", "esc\x1b[0m"),
    ("del_and_tilde", "tilde~ and DEL\x7f"),
    ("line_para_sep", "sep \u2028 para \u2029"),
    ("all_short_escapes", "\b\f\n\r\t"),
    ("high_control", "c1 \x1f end"),
    ("ampersand_only", "a&b=c"),
    ("lt_gt_amp", "<>&"),
]

MIXED_PAYLOADS = [
    ("nested_mixed", {"b": [1, 2.0, "x<y"], "a": {"c": None}}),
    ("sort_order", {"\u00e9": 1, "a": 2, "Z": 3}),
    ("bool_and_null", {"t": True, "f": False, "n": None}),
    ("deep_nesting", {"a": {"b": {"c": {"d": [1.5, "e&f"]}}}}),
    ("empty_containers", {"list": [], "map": {}}),
    ("int_vs_float", {"i": 1, "f": 1.0}),
    ("key_with_escaping", {'k"ey\\': "v", "k<ey": "w"}),
    ("many_float_forms", {"a": 0.0, "b": -0.0, "c": 1e16, "d": 1e-5, "e": 2.5}),
]


def vectors(payloads):
    out = []
    for label, payload in payloads:
        wrapped = {"v": payload}
        cb = canonical_json_bytes(wrapped)
        out.append(
            {
                "label": label,
                "payload": wrapped,
                "canonical_bytes": cb.decode("utf-8"),
                "digest": canonical_digest(wrapped),
            }
        )
    return out


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


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2

    doc = {
        "canonical_version": CANONICAL_VERSION,
        "generator": "testdata/generate_canonical_edge_vectors.py",
        "source_repo": "../shepherd",
        "source_commit": git_commit(REPO),
        "python_version": sys.version.split()[0],
        "edge_floats": vectors(FLOAT_PAYLOADS),
        "edge_strings": vectors(STRING_PAYLOADS),
        "mixed": vectors(MIXED_PAYLOADS),
    }

    dest = sys.argv[1]
    # newline="\n" matters: the pinned hash in golden_provenance_test.go is computed
    # over the LF form, so writing CRLF here would change it on Windows only.
    with open(dest, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(doc, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")

    print(f"wrote {dest}")
    print(f"  source_commit: {doc['source_commit'][:12]}")
    print(
        f"  floats={len(doc['edge_floats'])} "
        f"strings={len(doc['edge_strings'])} mixed={len(doc['mixed'])}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
