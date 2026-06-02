# Builds the goclaw CLAUDE runner image: the Go claude-runner plus the `claude`
# Code CLI it drives (brief §4, Option A′). Unlike the stub image this needs
# Node, since the SDK runs the claude CLI as a subprocess.
#
# Build from the repo root so the Go module is in context:
#   podman build -f container/claude.Containerfile -t goclaw-claude:latest .
#
# The host mounts the agent group's sessions dir at /sessions and passes
# ANTHROPIC_API_KEY in the environment. The runner serves every session subdir.

# --- build stage: compile the Go runner -------------------------------------
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/claude-runner ./cmd/claude-runner

# --- runtime stage: Node + claude CLI + the runner --------------------------
# node:22-slim gives us a small Debian base with Node for the claude CLI.
FROM docker.io/library/node:22-slim
# The pinned CLI version the SDK is verified against (keep in sync with the SDK's
# SupportedCLIVersion).
RUN npm install -g @anthropic-ai/claude-code@2.1.160 \
    && npm cache clean --force

COPY --from=build /out/claude-runner /usr/local/bin/claude-runner

# Run as a non-root uid that matches the host's --user 1000:1000 (brief §9).
# node:22-slim ships a "node" user at uid 1000; reuse it and give it a HOME the
# claude CLI can write its config/cache into.
USER 1000:1000
ENV HOME=/home/node
ENTRYPOINT ["/usr/local/bin/claude-runner", "-dir", "/sessions"]
