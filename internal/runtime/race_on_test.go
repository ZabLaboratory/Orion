//go:build race

package runtime

// raceEnabled reports whether this test binary was built with the
// race detector. The 20 k perf gate (perf20k_test.go) is a HARD
// wall-clock gate; the race build multiplies wall time 5–20× and
// would turn it into a flake generator, so the timing gate runs in
// the dedicated no-race `perf-20k` CI job instead (see
// .github/workflows/ci.yml). Dirty-cone SEMANTICS keep running under
// race (dirtycone_test.go).
const raceEnabled = true
