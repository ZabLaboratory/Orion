# Pinned Lumencast Go module for Orion

This directory contains the `lsml`, `protocol`, and `server` packages from
`Lumencast/lumencast-go` revision
`0c7cfc65c694ce4ab943eb4e7711fd226c671180`, copied unchanged from that
revision with its `go.mod`, `go.sum`, `LICENSE`, and `NOTICE`. Orion's `go.mod`
replaces only this exact module with this local path so its CI and Windows
sidecar build do not depend on a cross-organization private Git credential.

When the pinned module revision changes, replace these files from that revision
and run `go mod tidy` and the full Orion tests before updating the requirement.
