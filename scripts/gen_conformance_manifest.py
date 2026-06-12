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

    # ADR 007 §3.3: the `inline_only` flag is retired. The six core.db.*
    # clause atomics (from/where/join/select/order/limit) are ordinary
    # pure computes with real Orion executors (compute_db.go) and are
    # classified KindCompute by the conformance matrix — no special-casing
    # in the generated manifest.

    # Port/config signature fixture for the exec-port-parity gate
    # (internal/conformance/signatures.json). For each core node id we
    # emit the SET of port and config names the seed declares, so the
    # offline parity test can assert every string the Orion runtime
    # hardcodes exists in the authoring contract. Only names matter (the
    # gate checks identifiers, not types), so the fixture stays a flat set
    # per id — stable under type/description churn.
    signatures: dict[str, dict[str, list[str]]] = {}

    def _names(ports: object) -> list[str]:
        out = []
        if isinstance(ports, list):
            for p in ports:
                if isinstance(p, dict) and isinstance(p.get("name"), str):
                    out.append(p["name"])
        return sorted(set(out))

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
        entries.append(entry)
        sig = n.get("signature") or {}
        signatures[node_id] = {
            "inputs": _names(sig.get("inputs")),
            "outputs": _names(sig.get("outputs")),
            "config": _names(sig.get("config")),
        }
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

    sig_path = os.path.join(repo, "internal", "conformance", "signatures.json")
    sig_sorted = {k: signatures[k] for k in sorted(signatures)}
    with open(sig_path, "w", encoding="utf-8") as f:
        json.dump({"count": len(sig_sorted), "signatures": sig_sorted}, f, indent=2)
        f.write("\n")
    print(f"wrote {len(sig_sorted)} signatures to {sig_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
