# syntax=docker/dockerfile:1
#
# Control-plane image. The build context MUST be the repo root: the control
# plane imports wire-contract packages from sibling modules through the Go
# workspace (go.work) — `agentapi` from nodeagent and `guestwire` from
# guestagent (Phase 3) — so all module trees + go.work(.sum) must be present at
# build time. go.work lists every workspace module, and `go mod download`
# validates the whole graph, so guestagent's go.mod/go.sum must be copied too
# even though the control plane only links its dependency-free api package.
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
COPY guestagent/go.mod guestagent/go.sum ./guestagent/
RUN go mod download

# Sources.
COPY controlplane/ ./controlplane/
COPY nodeagent/ ./nodeagent/
COPY guestagent/ ./guestagent/
RUN go build -ldflags="-s -w" -o /out/controlplane ./controlplane/cmd/controlplane

# Root (not :nonroot): the Phase-1 file-backed secrets store writes to
# PROTEOS_SECRETS_FILE at startup, which lives on the mounted data volume.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/controlplane /usr/local/bin/controlplane
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/controlplane", "-migrate"]
