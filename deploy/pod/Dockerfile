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
    CGO_ENABLED=0 go build -trimpath -o /out/browsercheck ./cmd/browsercheck && \
    (cd /out && sha256sum worker controller browsercheck > tool-pins.txt)

# ---- Stage 2: runtime ----------------------------------------------------
FROM debian:bookworm-slim

ARG HERMES_FORK_REF=dbb70f612d0146002b8815f90e8391d202626c9f
ARG CLAUDE_CODE_VERSION=2.1.220
ARG CODEX_VERSION=0.146.0
ARG PNPM_VERSION=9.15.4

RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      ca-certificates curl git tmux python3 python3-pip python3-venv \
      bubblewrap jq openssh-client \
      chromium fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/google/chrome \
    && test -x /usr/lib/chromium/chromium \
    && ln -s /usr/lib/chromium/chromium /opt/google/chrome/chrome

# Node 22 (validation toolchain of node consumers) + pinned pnpm.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y -qq nodejs && rm -rf /var/lib/apt/lists/* \
    && npm install --global --no-audit --no-fund pnpm@${PNPM_VERSION} \
       @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
       @openai/codex@${CODEX_VERSION}

# The Hermes fork, pinned by commit. Hermes refuses wheel builds by
# design; the editable install against a pinned checkout is the supported
# from-source path.
RUN git clone --filter=blob:none https://github.com/RHEMS-Japan/hermes-agent /opt/hermes-src \
    && git -C /opt/hermes-src checkout --quiet "${HERMES_FORK_REF}" \
    && python3 -m venv /opt/hermes \
    && /opt/hermes/bin/pip install --no-cache-dir -e /opt/hermes-src \
    && ln -s /opt/hermes/bin/hermes /usr/local/bin/hermes

# Pre-build the dashboard SPA (vite writes it to hermes_cli/web_dist) so
# `hermes dashboard --skip-build` can serve the board UI without a runtime
# npm build. npm resolves the workspace root's lockfile and installs into
# the ROOT node_modules (web/ gets none), so the cleanup names both trees
# by absolute path in the same layer; the editable install only needs the
# sources plus the built dist.
RUN cd /opt/hermes-src/web && npm ci --no-audit --no-fund \
    && npm run build \
    && rm -rf /opt/hermes-src/node_modules /opt/hermes-src/web/node_modules

COPY --from=engine /out/attendant /out/runner /out/worker /out/controller /out/browsercheck /usr/local/bin/
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
