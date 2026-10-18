# Active editable defaults read-back

GET /api/v1/show/editable-preview/state now returns live defaults alongside active author identity. It holds the Host lane lease, waits for queued actor inputs, captures state, and checks actor identity/sequence/role epoch remained stable. It publishes no LSML mutation or snapshot and stores no inactive scene.

The native proof checks a Program PUT reaches solar/lane-a width32, followed by GET edit_seq1/default32; Preview B remains unchanged. The socket test checks GET anonymous403, retired scene/old role409 and invalid target400. Tests for affected packages and all editable API guards pass; go vet of affected packages passes. The new packaged descriptor is separately probed in the accompanying log.

Final binary SHA256 befcce6e95846d8ecf4b736a7889c69412c6584edcaf51ea7b12bf103beb837e supersedes the C9 candidate from the initial report. Contract, artifact and Solar package3133269c were delivered to Main 1, who owns the common refresh and visible CEF validation. This unit did not restart or mutate the common runtime.

Source parent a052866 plus file hashes in companion provenance. Continuation worktree/branch remain unchanged.
