# syntax=docker/dockerfile:1
#
# Control-plane image. The build context MUST be the repo root: the control
# plane imports the `agentapi` wire-contract package from the nodeagent module
# through the Go workspace (go.work), so both module trees + go.work(.sum) have
# to be present at build time. nodeagent has no external deps (the agentapi
# package is pure stdlib types), so this stays light.
#
# Migrations are embedded in the binary (controlplane/internal/store + iofs), so
# the runtime image is just the static binary — `-migrate` applies them on boot.

FROM golang:1.26 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath

# Module graph first, for layer caching.
COPY go.work go.work.sum ./
COPY controlplane/go.mod controlplane/go.sum ./controlplane/
COPY nodeagent/go.mod ./nodeagent/
RUN go mod download

# Sources.
COPY controlplane/ ./controlplane/
COPY nodeagent/ ./nodeagent/
RUN go build -ldflags="-s -w" -o /out/controlplane ./controlplane/cmd/controlplane

# Root (not :nonroot): the Phase-1 file-backed secrets store writes to
# PROTEOS_SECRETS_FILE at startup, which lives on the mounted data volume.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/controlplane /usr/local/bin/controlplane
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/controlplane", "-migrate"]
