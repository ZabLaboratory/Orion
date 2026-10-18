# Cross-language hashBundle golden — reproduction harness

Gate for **ADR 007 §C4** (adopt-on-verify). Verifies that Go
`lsml.HashBundle` agrees byte-for-byte with the TS reference
`@lumencast/compiler.hashBundle` (`canonicalize.ts`) for the same
logical LSML 1.1 bundle.

The encoded golden values live in
`../../hash_xlang_golden_test.go`. This directory documents how they
were captured so they can be regenerated when the SDK fix lands.

## Bundles under test

| Case | Risk | Verdict |
|---|---|---|
| `case_a_float` | exponential / shortest-decimal floats, >2^53 int | MATCH |
| `case_b_html` | strings with `&` `<` `>` | MATCH (since fix, 2026-06-05) |
| `case_c_optional_absent` | optional field absent vs `omitempty` | MATCH |

## Resolved divergence (case_b_html)

Go `encoding/json` escaped `&`→`&`, `<`→`<`, `>`→`>` by
default (`SetEscapeHTML(true)`); TS `JSON.stringify` does not. Same §3
discipline on paper, divergent serializers in practice.

Fixed in `lsml/hash.go` by routing every string emission (both object
values and object keys) through `marshalString`, which uses a
`json.Encoder` with `SetEscapeHTML(false)`. The canonical form is now
byte-identical to `canonicalize.ts`. `case_b_html` is a permanent
regression golden locking the TS hash.

## Regenerate the TS golden

```bash
# TS side (Node >= 22), from a checkout of lumencast-js with compiler built:
node -e '
import("@lumencast/compiler").then(async ({canonicalize, ZERO_HASH}) => {
  const fs = await import("node:fs");
  const b = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  const canon = canonicalize({ ...b, scene_version: ZERO_HASH });
  const dg = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(canon));
  const hex = [...new Uint8Array(dg)].map(x=>x.toString(16).padStart(2,"0")).join("");
  console.log(JSON.stringify({ hash: hex, canonical: canon }, null, 2));
})' bundle.json
```

```bash
# Go side, from lumencast-go root:
go test ./lsml/ -run HashBundle_CrossLanguageGolden -v
```
