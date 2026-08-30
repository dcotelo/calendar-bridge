# syntax=docker/dockerfile:1

# Base images are pinned by digest, not by tag: a tag is mutable, so pinning
# only the tag means a rebuild can silently pull a different base. Dependabot's
# docker ecosystem is configured for this repo and will open a PR when either
# digest moves.
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /src

# Dependencies first, so a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/calendar-bridge ./cmd/calendar-bridge

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /app
COPY --from=build /out/calendar-bridge /app/calendar-bridge

# Runs as UID 65532 (distroless "nonroot"). The mounted config and secrets must
# be readable by that UID — see docs/deployment/docker.md, which is the single
# most common first-run failure.
USER 65532:65532

# The image carries no config and no secrets: both are mounted at runtime from
# a Docker volume, a Fly volume, or a Kubernetes Secret. Note that the paths
# inside config.yaml are resolved against WORKDIR (/app), so use ABSOLUTE paths
# such as /app/config/secrets/personal-token.json.
VOLUME ["/app/config"]

ENTRYPOINT ["/app/calendar-bridge"]
CMD ["run", "-config", "/app/config/config.yaml"]
