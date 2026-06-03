# Builds the goclaw CLAUDE runner image: the Go claude-runner plus the `claude`
# Code CLI it drives (brief §4, Option A′), on top of a development environment
# (git, gh, build-essential, Python3, Go, ripgrep, jq, ...) so the agent can do
# real work, including opening pull requests. Needs Node, since the SDK runs the
# claude CLI as a subprocess.
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

# --- runtime stage: a dev environment + Node + claude CLI + the runner -------
# node:22-slim gives us a small Debian base with Node for the claude CLI. The
# agent does real development work (clone, build, script), so the image carries a
# solid toolchain, not just Node.
FROM docker.io/library/node:22-slim

# Pin a Go version to install from the official tarball (Debian's apt golang lags).
ARG GO_VERSION=1.26.3

# Core dev toolchain. ca-certificates + git for VCS over HTTPS; build-essential
# for native/C deps; python3 + pip for scripting; ripgrep/jq/fd/etc. for the
# agent's day-to-day. Cleaned up to keep the layer lean.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        curl \
        wget \
        build-essential \
        python3 \
        python3-pip \
        python3-venv \
        ripgrep \
        jq \
        unzip \
        zip \
        vim \
        less \
        procps \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI (gh) from GitHub's apt repo, so the agent can open pull requests
# (gh pr create) and fork repos (gh repo fork). Authenticated at run time via the
# GH_TOKEN env the host injects from GOCLAW_GITHUB_TOKEN.
RUN set -eux; \
    mkdir -p -m 755 /etc/apt/keyrings; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends gh; \
    rm -rf /var/lib/apt/lists/*

# Go toolchain, fetched for the image's architecture (arm64 on Apple Silicon,
# amd64 on Intel) so the same Containerfile works on either.
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    url="https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; \
    curl -fsSL "$url" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz
ENV PATH="/usr/local/go/bin:${PATH}"
# GOPATH/GOCACHE under /work so module/build caches are writable by uid 1000 and
# stay out of the vault.
ENV GOPATH=/work/go GOCACHE=/work/.gocache

# The pinned CLI version the SDK is verified against (keep in sync with the SDK's
# SupportedCLIVersion).
RUN npm install -g @anthropic-ai/claude-code@2.1.160 \
    && npm cache clean --force

COPY --from=build /out/claude-runner /usr/local/bin/claude-runner

# /work is the agent's scratch working directory: clones, temp files, and any
# command output land here, NOT in the mounted /vault (which would pollute it).
# Ephemeral with the container; owned by the runtime uid so the agent can write.
RUN mkdir -p /work && chown -R 1000:1000 /work

# Run as a non-root uid that matches the host's --user 1000:1000 (brief §9).
# node:22-slim ships a "node" user at uid 1000; reuse it and give it a HOME the
# claude CLI can write its config/cache into.
USER 1000:1000
ENV HOME=/home/node
ENTRYPOINT ["/usr/local/bin/claude-runner", "-dir", "/sessions"]
