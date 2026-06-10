#!/usr/bin/env python3
"""Regenerate internal/conformance/manifest.json from Blue's source of truth.

ADR 003 §6 criterion 1 (total conformance). The conformance matrix
asserts every Blue compute-manifest node id has a registered Orion
executor + a passing execution test. That matrix runs OFFLINE in CI
(no Blue service), so the canonical node-id set is vendored in-tree as
`internal/conformance/manifest.json`.

This script re-derives that file directly from Blue's seeded stdlib and
its purity map — the same sources `GET /_compute-manifest` serves at
runtime — so the vendored copy can never silently drift from the
authoring language. Run it from a checkout where Blue is reachable on
the path (sibling repo at ../../../Blue), then commit the diff.

    python scripts/gen_conformance_manifest.py [path-to-Blue/src]

The default Blue source path assumes the étage-1 sibling layout
(`<structure>/Blue/src`). Pass an explicit path otherwise.

When this script changes the output, a Blue node type was added,
removed, or re-classified. That is a deliberate cross-repo event: the
new id either gains an Orion executor + test, or lands on
`conformance_allowlist.txt` with a documented reason — never silently.
"""

import json
import os
import sys


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    repo = os.path.dirname(here)
    default_blue_src = os.path.normpath(
        os.path.join(repo, "..", "..", "Blue", "src")
    )
    blue_src = sys.argv[1] if len(sys.argv) > 1 else default_blue_src
    if not os.path.isdir(blue_src):
        print(f"error: Blue src not found at {blue_src}", file=sys.stderr)
        return 2
    sys.path.insert(0, blue_src)

    from blue.models.canonical import CANONICAL_EVENT_TYPES
    from blue.services import stdlib_seeder
    from blue.services.node_purity import purity_for

    # Inline-only atoms (ADR 006 §3.5 / issue #107): core.db.*
    # query-builder atoms that Blue's executor contract restricts to
    # core.db.query's config.inline_graph. They are served transitively
    # by OpDBQuery and are NOT standalone-executable (Blue raises
    # db_node_outside_query for a main-graph placement). The generator
    # marks them inline_only=true so the vendored manifest reflects this
    # classification — the conformance matrix classifies them KindInlineOnly.
    _INLINE_ONLY_IDS = frozenset(
        {
            "core.db.from@1",
            "core.db.join@1",
            "core.db.limit@1",
            "core.db.order@1",
            "core.db.select@1",
            "core.db.where@1",
        }
    )

    entries = []
    for n in stdlib_seeder._CORE_NODES:
        pur = purity_for(n["namespace"], n["name"])
        node_id = f"{n['namespace']}.{n['name']}@1"
        entry = {
            "node_id": node_id,
            "namespace": n["namespace"],
            "name": n["name"],
            "version": 1,
            "category": n.get("category"),
            "is_pure": pur["is_pure"],
            "is_bounded": pur["is_bounded"],
            "source": "stdlib",
            "platform": None,
        }
        if node_id in _INLINE_ONLY_IDS:
            entry["inline_only"] = True
        entries.append(entry)
    for event_type, _model in CANONICAL_EVENT_TYPES:
        entries.append(
            {
                "node_id": f"quasar.twitch.{event_type}@1",
                "namespace": "quasar.twitch",
                "name": event_type,
                "version": 1,
                "category": "platform-event",
                "is_pure": True,
                "is_bounded": True,
                "source": "stdlib",
                "platform": {"name": "twitch"},
            }
        )

    entries.sort(key=lambda e: e["node_id"])
    out_path = os.path.join(repo, "internal", "conformance", "manifest.json")
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump({"count": len(entries), "entries": entries}, f, indent=2)
        f.write("\n")
    print(f"wrote {len(entries)} entries to {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
