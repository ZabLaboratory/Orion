# Local optimization / regression contract

Scope: Orion typed editable Preview compilation. Solar DOM target caching and
Prism editable snapshot/diff optimizations are validated in their own worktrees.
Pulsar, authentication, wire formats, signed artifact bytes and asset policy are
unchanged. No deployment or package publication is part of this increment.

| Criterion | Risk | Command / proof |
| --- | --- | --- |
| Typed output equals the existing serialized path | Assets, transitions, defaults lost | Compiler equivalence tests, serialized JSON equality |
| Raw precompiled bytes remain untouched | Digest drift | Byte identity test, malformed-root error parity |
| Preview retains error and sequence contracts | Silent fallback / invalid activation | API tests and full Go suite |
| Avoid a complete encode/decode | Cost merely moved elsewhere | Paired benchmark, time and allocated bytes |
| No shared mutable defaults introduced | Runtime mutates immutable bundle | Ownership tests |

Base: cd2fea40fd9471a9e11153025e82909783945a14.
