# The single-pod runtime image (docs/RUNTIME_POD.md).
# Build for linux/arm64 (the cluster's node architecture).

# ---- Stage 1: engine binaries -------------------------------------------
FROM golang:1.25-bookworm AS engine
ARG ENGINE_SHA
WORKDIR /src
COPY . .
RUN test -n "$ENGINE_SHA" && \
    CGO_ENABLED=0 go build -trimpath -o /out/attendant ./cmd/attendant && \
    CGO_ENABLED=0 go build -trimpath -o /out/runner ./cmd/runner && \
    CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -o /out/controller ./cmd/controller && \
    (cd /out && sha256sum worker controller > tool-pins.txt)

# ---- Stage 2: runtime ----------------------------------------------------
FROM debian:bookworm-slim

ARG HERMES_FORK_REF=dbb70f612d0146002b8815f90e8391d202626c9f
ARG CLAUDE_CODE_VERSION=2.1.220
ARG CODEX_VERSION=0.146.0
ARG PNPM_VERSION=9.15.4

RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      ca-certificates curl git tmux python3 python3-pip python3-venv \
      bubblewrap jq openssh-client \
    && rm -rf /var/lib/apt/lists/*

# Node 22 (validation toolchain of node consumers) + pinned pnpm.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y -qq nodejs && rm -rf /var/lib/apt/lists/* \
    && npm install --global --no-audit --no-fund pnpm@${PNPM_VERSION} \
       @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
       @openai/codex@${CODEX_VERSION}

# The Hermes fork, pinned by commit.
RUN python3 -m venv /opt/hermes \
    && /opt/hermes/bin/pip install --no-cache-dir \
       "git+https://github.com/RHEMS-Japan/hermes-agent@${HERMES_FORK_REF}" \
    && ln -s /opt/hermes/bin/hermes /usr/local/bin/hermes

COPY --from=engine /out/attendant /out/runner /out/worker /out/controller /usr/local/bin/
COPY --from=engine /out/tool-pins.txt /etc/lassdas/tool-pins.txt
COPY deploy/pod/entrypoint.sh /usr/local/bin/lassdas-entrypoint
RUN chmod 0755 /usr/local/bin/lassdas-entrypoint

# Non-root. UID separation for agents (docs/RUNTIME_POD.md Phase-3 gate)
# is layered on top: agents run as 'agent', the engine as 'lassdas'.
RUN useradd --create-home --uid 1000 lassdas \
    && useradd --create-home --uid 2000 agent
USER lassdas
WORKDIR /home/lassdas

ENTRYPOINT ["/usr/local/bin/lassdas-entrypoint"]
