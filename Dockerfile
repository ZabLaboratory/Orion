# syntax=docker/dockerfile:1.7

# ---- build stage ----------------------------------------------------------
FROM golang:1.26-alpine AS build

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
    go mod download

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
FROM gcr.io/distroless/static:nonroot

# Distroless static: orion only. .env.template stays in the repo (docs
# only); it never makes it into the image because rsync's
# `--exclude .env.*` filter would skip it on deploy anyway.
COPY --from=build /out/orion        /orion
COPY --from=build /out/healthcheck  /healthcheck

# OCI image labels — re-declared in the runtime stage (ARGs do not cross
# stage boundaries) so they land on the final image, not the build stage.
ARG ORION_GIT_REVISION=unknown
ARG ORION_GIT_SOURCE=https://github.com/ZabLaboratory/Orion
LABEL org.opencontainers.image.revision="${ORION_GIT_REVISION}"
LABEL org.opencontainers.image.source="${ORION_GIT_SOURCE}"

USER nonroot:nonroot
EXPOSE 4007 4017
ENTRYPOINT ["/orion"]
