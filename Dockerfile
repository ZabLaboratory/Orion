# syntax=docker/dockerfile:1.7

# ---- build stage ----------------------------------------------------------
FROM golang:1.26-alpine AS build

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

# Goose migration runner — installed alongside so the runtime image can
# run migrations without a separate sidecar. Pinned to a release tag so
# a silent breaking change upstream doesn't sneak into prod.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go install github.com/pressly/goose/v3/cmd/goose@v3.22.1

# ---- runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

# Distroless static: orion + goose + migrations.
# The compose run with --entrypoint /goose runs migrations; the default
# entrypoint /orion is the service itself. .env.template stays in the
# repo (docs only); it never makes it into the image because rsync's
# `--exclude .env.*` filter would skip it on deploy anyway.
COPY --from=build /out/orion        /orion
COPY --from=build /go/bin/goose     /goose
COPY migrations                     /migrations

USER nonroot:nonroot
EXPOSE 4007 4017
ENTRYPOINT ["/orion"]
