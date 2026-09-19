# File size policy

The default ceiling is **1,000 lines per text file**, including tests and documentation.
New feature modules must stay below this ceiling. Do not compress or minify source to pass.

`.file-size-exceptions.json` lists every existing oversized file individually, with its
reviewable justification and current ceiling. Existing exceptions cannot grow unnoticed:
raising a ceiling requires an explicit review of that file's justification. Historical
design snapshots, generated lockfiles, exhaustive contract catalogues, coherent regression
scenarios and stateful lifecycle roots are distinguished from new modules.

Prism's broadcast engine is an explicit exception: preserve atomic lane transactions.
Stateful UI closures retained in this pass must not change callback/hook ownership simply
to reduce file length. Their exception is not permission to add unrelated features there.

Run `python scripts/check_file_sizes.py`; CI runs the same guard and its self-tests.
The guard examines tracked and unignored new files. Binary assets and generated E2E/evidence
directories are excluded because they are proof artifacts, not hand-maintained source.
All other UTF-8 text files are checked.

This inventory is explicit technical debt, not a claim that every existing source file is
already below 1,000 lines. Remove entries after independently validated feature extraction;
never weaken functional tests to make a split pass.
