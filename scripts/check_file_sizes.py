#!/usr/bin/env python3
"""Bound handwritten/text files; preserve reviewed, individually justified exceptions."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

LIMIT = 1000
# Generated test evidence is retained as-is, not edited to satisfy a source-size rule.
EVIDENCE_PREFIXES = ("evidence/", "e2e/__artifacts__/", "e2e/evidence/")


def line_count(content: str) -> int:
    return content.count("\n") + int(bool(content) and not content.endswith("\n"))


def violation(path: str, count: int, exceptions: dict[str, dict[str, object]]) -> str | None:
    item = exceptions.get(path)
    if item is not None:
        reason = item.get("reason")
        ceiling = item.get("max_lines")
        if not isinstance(reason, str) or not reason.strip():
            return f"{path}: exception requires a reason"
        if not isinstance(ceiling, int) or isinstance(ceiling, bool) or ceiling <= LIMIT:
            return f"{path}: invalid exception ceiling"
        if count > ceiling:
            return f"{path}: {count} lines exceeds reviewed ceiling {ceiling}"
    elif count > LIMIT:
        return f"{path}: {count} lines exceeds {LIMIT}; split by feature or justify explicitly"
    return None


def self_test() -> None:
    assert line_count("") == 0
    assert line_count("a\n") == line_count("a") == 1
    assert line_count("a\nb") == 2
    assert violation("new.ts", 1000, {}) is None
    assert violation("new.ts", 1001, {}) is not None
    approved = {"old.ts": {"max_lines": 1200, "reason": "Transactional lifecycle"}}
    assert violation("old.ts", 1200, approved) is None
    assert violation("old.ts", 1201, approved) is not None
    assert violation("old.ts", 1200, {"old.ts": {"max_lines": 1200, "reason": ""}})
    print("File-size guard self-tests passed.")


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    exceptions = json.loads((root / ".file-size-exceptions.json").read_text(encoding="utf-8"))
    names = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=root
    ).decode("utf-8").split("\0")
    failures: list[str] = []
    checked = 0
    for name in sorted(set(names) - {""}):
        if name.startswith(EVIDENCE_PREFIXES):
            continue
        path = root / name
        if not path.is_file():  # Submodules and locally removed tracked files.
            continue
        data = path.read_bytes()
        if b"\0" in data:
            continue
        try:
            source = data.decode("utf-8-sig")
        except UnicodeDecodeError:  # Binary assets, never count bytes as source lines.
            continue
        checked += 1
        problem = violation(name, line_count(source), exceptions)
        if problem:
            failures.append(problem)
    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"File-size guard: {checked} text files, {len(exceptions)} reviewed exceptions, no growth.")
    return 0


if __name__ == "__main__":
    if "--self-test" in sys.argv:
        self_test()
    else:
        raise SystemExit(main())
