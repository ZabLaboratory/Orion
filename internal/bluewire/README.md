# Blue projection bridges

Orion owns the per-slot bridge registry here. Bridge projects Blue outputs onto
an existing SceneMirror; Registry owns the paced loops, operator forwarding and
native LSML mutation seam. Host owns execution, Hub owns physical lane routing.
`authority.go` joins both loops then reuses their bridge, sequence, actor and
mirror identities in their new logical role. No StepOnce or startup gate replay
occurs. Admission leases fence operator/platform consumers across this exchange.
Tests beside Bridge/Registry cover projection, ordering, lifecycle and authority.
The native receipt contract is documented in [api](../api/README.md).
