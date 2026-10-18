# Published LEC/LCK visual regression program

`scene-lec-lck.program.json` contains the exact Blue program bytes synchronized
by Prism for scene `8cbef8cd-ed05-42ca-af9c-7e2762e860b8`, revision
`e3a5bc45-4167-47f7-ad8c-4708fd3a1c32`. It declares `LEC`, `LCK` and
`load-match`; it is not a synthetic output program. Its digest matches the
cached ZabCanvas reference. The program loader verifies its canonical digest.

Program digest: `sha256:ca2f21db4459f616c0f996fb7c03b3cf61e2a81d48bbc870d671f8fbda327bfc`.
Exact byte SHA-256: `9754a0b16acbaa5f061e9b45189b9a21443deb97ed37449d277b339f77a2b7b3`.

`TestRealNativeOperatorVisual` invokes the real operator HTTP route and its
read-only DB effects. It requires an authenticated read gateway, the actual
native receiver and a full LSML scene. The CEF observer separately proves its
rendered result. This fixture does not certify current remote publication or
the local visual variant's ZabCanvas compatibility attestation.
