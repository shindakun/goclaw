# Builds the goclaw runner image. For now it packages cmd/stub-runner (the echo
# stand-in); the real agent-runner will replace the final stage later (brief §4).
#
# Build from the repo root so the Go module is in context:
#   podman build -f container/runner.Containerfile -t goclaw-runner:latest .
#
# One runner serves a whole agent group: the host mounts the group's sessions
# directory at /sessions, and the runner serves every session subdir beneath it
# (each holding inbound.db + outbound.db) (brief §3.1, §3.3, §5.1).

# --- build stage ------------------------------------------------------------
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary (CGO disabled - modernc.org/sqlite is pure Go).
COPY . .
RUN CGO_ENABLED=0 go build -o /out/stub-runner ./cmd/stub-runner

# --- runtime stage ----------------------------------------------------------
# Minimal, non-root. uid 1000 matches the host's --user 1000:1000 (brief §9).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/stub-runner /usr/local/bin/stub-runner
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/stub-runner", "-dir", "/sessions"]
