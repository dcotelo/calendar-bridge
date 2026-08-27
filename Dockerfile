# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/calendar-bridge ./cmd/calendar-bridge

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/calendar-bridge /app/calendar-bridge
# Config and per-account secrets are mounted at runtime (Fly.io volume,
# Docker volume, or k8s Secret) — never baked into the image.
ENTRYPOINT ["/app/calendar-bridge"]
CMD ["run", "-config", "/app/config/config.yaml"]
