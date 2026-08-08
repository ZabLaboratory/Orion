package store

import "testing"

// The advisory-lock key is a value two independent processes must agree on
// without ever talking. Pinning it makes a change to the derivation — or to
// the name it derives from — a red test rather than a silent loss of the
// mutual exclusion (RC 44).
func TestServiceTokenLockKey_IsPinned(t *testing.T) {
	const want = int64(-3799780643422053987)
	if got := ServiceTokenLockKey(); got != want {
		t.Fatalf("ServiceTokenLockKey() = %d, want %d — the key changed, so a running Orion and a new one would no longer exclude each other", got, want)
	}
	if ServiceTokenLockName != "orion.service_token.rotator.v1" {
		t.Fatalf("ServiceTokenLockName = %q; the key is derived from it", ServiceTokenLockName)
	}
}

// Release is deferred unconditionally at the call site, including on the paths
// where no lock was taken.
func TestProcessLock_ReleaseIsSafeOnNil(t *testing.T) {
	var l *ProcessLock
	l.Release()
	(&ProcessLock{}).Release()
}
