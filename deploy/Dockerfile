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

# Static binary, no CGO. ldflags strip debug info to keep the image tight.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/orion ./cmd/orion

# ---- migrations baked into a small artefact directory --------------------
FROM alpine:3.20 AS assets
WORKDIR /assets
COPY migrations /assets/migrations
COPY .env.template /assets/.env.template

# ---- runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

# Distroless static contains no shell, no apt, no busybox — only the
# binary + ca-certificates. The build never wrote any other file into
# /out so we copy just the binary.
COPY --from=build /out/orion /orion
COPY --from=assets /assets/migrations /migrations
COPY --from=assets /assets/.env.template /.env.template

USER nonroot:nonroot
EXPOSE 4007 4017
ENTRYPOINT ["/orion"]
