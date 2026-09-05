# The single-pod runtime image (docs/RUNTIME_POD.md).
# Build for linux/arm64 (the cluster's node architecture).

# ---- Stage 1: engine binaries -------------------------------------------
FROM golang:1.26-bookworm AS engine
ARG ENGINE_SHA
WORKDIR /src
COPY . .
RUN test -n "$ENGINE_SHA" && \
    CGO_ENABLED=0 go build -trimpath -o /out/attendant ./cmd/attendant && \
    CGO_ENABLED=0 go build -trimpath -o /out/runner ./cmd/runner && \
    CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -o /out/controller ./cmd/controller && \
    CGO_ENABLED=0 go build -trimpath -o /out/browsercheck ./cmd/browsercheck && \
    CGO_ENABLED=0 go build -trimpath -o /out/statusboard ./cmd/statusboard && \
    (cd /out && sha256sum worker controller browsercheck > tool-pins.txt)

# ---- Stage 2: runtime ----------------------------------------------------
FROM debian:bookworm-slim

ARG HERMES_FORK_REF=dbb70f612d0146002b8815f90e8391d202626c9f
ARG CLAUDE_CODE_VERSION=2.1.220
ARG CODEX_VERSION=0.146.0
ARG PNPM_VERSION=9.15.4
ARG KUBECTL_VERSION=v1.35.8
ARG KUBECTL_SHA256=cc749967b62f4422260bc9c0aa7a7c55f45175ae38cb8d95767b5d2b7e04c1fd
ARG AWSCLI_VERSION=2.36.40
ARG AWSCLI_SHA256=f26e08d0d64d9f35f304a9df94d63cbd11ffe8881c8b4bf60be934cf9e4d13a9

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

# Read-only observation tools for the investigating designer's exec probes
# (docs/INVESTIGATING_DESIGNER.md §3.2): kubectl and the AWS CLI, pinned by
# version and checksum. The image is linux/arm64 (the release script builds
# that platform); the guard makes any other architecture fail here rather
# than at the first probe. Neither tool holds a credential: the identities
# reach them through the kubeconfig and the role pointers the kernel passes
# to exec probes (internal/probe ExecEnvironmentNames).
RUN test "$(uname -m)" = "aarch64" \
    && curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/arm64/kubectl" \
    && echo "${KUBECTL_SHA256}  /usr/local/bin/kubectl" | sha256sum -c - \
    && chmod 0755 /usr/local/bin/kubectl \
    && curl -fsSL -o /tmp/awscli.zip "https://awscli.amazonaws.com/awscli-exe-linux-aarch64-${AWSCLI_VERSION}.zip" \
    && echo "${AWSCLI_SHA256}  /tmp/awscli.zip" | sha256sum -c - \
    && apt-get update -qq && apt-get install -y -qq --no-install-recommends unzip \
    && rm -rf /var/lib/apt/lists/* \
    && unzip -q /tmp/awscli.zip -d /tmp/awscli \
    && /tmp/awscli/aws/install --bin-dir /usr/local/bin --install-dir /usr/local/aws-cli \
    && rm -rf /tmp/awscli /tmp/awscli.zip \
    && kubectl version --client >/dev/null \
    && aws --version

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

# The Go toolchain rides along for consumers whose deterministic validation
# builds Go (the gateway repo's checks run `go` inside the validation
# sandbox; a missing toolchain kills validation before it can record
# anything).
COPY --from=engine /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH

COPY --from=engine /out/attendant /out/runner /out/worker /out/controller /out/browsercheck /out/statusboard /usr/local/bin/
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
