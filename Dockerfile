# syntax=docker/dockerfile:1.7

# ---- build stage ----------------------------------------------------------
# Digest verified against the registry on 2026-08-14. Update only through a
# reviewed toolchain change so the production compiler cannot drift by tag.
FROM golang:1.26-alpine@sha256:70b46548e42db77e0966aaf3619fd068734dc6c77584d526b91126504fd95816 AS build

# The private Blue module is fetched through Git over HTTPS. The official
# Alpine Go image does not include Git, so install only the client and CA
# roots needed for the module-download step; the runtime stage remains
# distroless and contains neither Git nor the build credentials.
RUN apk add --no-cache git ca-certificates

# OCI provenance. Fed by the deploy/build pipeline so every image on the
# VPS is traceable to a commit + repo (the running prod image carried no
# revision/source label, which is what let a stale tree drift in silently).
# Both default to "unknown" so a bare `docker build` still succeeds.
ARG ORION_GIT_REVISION=unknown
ARG ORION_GIT_SOURCE=https://github.com/ZabLaboratory/Orion

WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=blue_read_token,required \
    set -eu; \
    token="$(cat /run/secrets/blue_read_token)"; \
    git config --global "url.https://x-access-token:${token}@github.com/ZabLaboratory/.insteadOf" "https://github.com/ZabLaboratory/"; \
    cleanup() { git config --global --unset "url.https://x-access-token:${token}@github.com/ZabLaboratory/.insteadOf" || true; }; \
    trap cleanup EXIT; \
    GIT_TERMINAL_PROMPT=0 GOPRIVATE=github.com/ZabLaboratory/* GONOSUMDB=github.com/ZabLaboratory/* go mod download

COPY . .

# Static orion binary (no CGO, stripped).
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/orion ./cmd/orion

# Tiny static liveness probe — the distroless runtime has no shell/curl, so
# Docker's HEALTHCHECK self-execs this binary instead. See cmd/healthcheck.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

# Goose migration runner — RETIRED (#15, #331, ADR-BLUE-012 §4.3, Bastion
# C3): Orion holds no database and no migrations/ directory anymore.

# ---- runtime stage --------------------------------------------------------
# Digest verified against the registry on 2026-08-14. The runtime base is
# pinned independently from the Go build image.
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

# Distroless static: orion only. .env.template stays in the repo (docs
# only); it never makes it into the image because rsync's
# `--exclude .env.*` filter would skip it on deploy anyway.
COPY --from=build /out/orion        /orion
COPY --from=build /out/healthcheck  /healthcheck

# OCI image labels — re-declared in the runtime stage (ARGs do not cross
# stage boundaries) so they land on the final image, not the build stage.
ARG ORION_GIT_REVISION=unknown
ARG ORION_GIT_SOURCE=https://github.com/ZabLaboratory/Orion
ARG ORION_GO_VERSION=unknown
LABEL org.opencontainers.image.revision="${ORION_GIT_REVISION}"
LABEL org.opencontainers.image.source="${ORION_GIT_SOURCE}"
LABEL org.opencontainers.image.build.base.name="golang:1.26-alpine"
LABEL org.opencontainers.image.build.base.digest="sha256:70b46548e42db77e0966aaf3619fd068734dc6c77584d526b91126504fd95816"
LABEL org.opencontainers.image.base.name="gcr.io/distroless/static:nonroot"
LABEL org.opencontainers.image.base.digest="sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6"
LABEL org.opencontainers.image.build.go-version="${ORION_GO_VERSION}"

USER nonroot:nonroot
EXPOSE 4007 4017
ENTRYPOINT ["/orion"]
