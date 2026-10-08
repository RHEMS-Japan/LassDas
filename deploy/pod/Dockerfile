# Runtime image for the standalone engine and configured role tools.
# The golang stage is the Go version read by the CI image-version check.
# Build natively for linux/arm64; package.sh builds the three engine commands.
FROM golang:1.26-bookworm AS bundle
WORKDIR /src
COPY rewrite /src/rewrite
RUN mkdir -p /out && GOMAXPROCS=2 sh /src/rewrite/package.sh /out/bundle

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
# The role syscall profile is upstream Moby's, pinned by commit and checksum
# like the tools above. It is policy input for the runtime's compile step,
# not a profile this image applies to anything.
ARG MOBY_SECCOMP_REF=85e237f1fe229a0c61c9c7d8e743fa780d3b97ca
ARG MOBY_SECCOMP_SHA256=785b2429264afba4d594320337cb17f144f3c7d51585f9805eef72e28f4f9334

RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      ca-certificates curl git tmux python3 python3-pip python3-venv \
      bubblewrap jq openssh-client \
      chromium fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/google/chrome \
    && test -x /usr/lib/chromium/chromium \
    && ln -s /usr/lib/chromium/chromium /opt/google/chrome/chrome

# The firewall tool of deploy/ticket-engine: that Pod's network init
# containers can run this image's iptables (the nf_tables backend,
# /usr/sbin/xtables-nft-multi) with NET_ADMIN to write the Pod's own egress
# rules, instead of a second image every node must also pull. On a line of its
# own and without -qq, so the build log names the packages it adds and the
# space they take.
RUN apt-get update -qq && apt-get install -y -q --no-install-recommends iptables \
    && rm -rf /var/lib/apt/lists/* \
    && test -x /usr/sbin/xtables-nft-multi

# Node 22 (validation toolchain of node consumers) + pinned pnpm.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y -qq nodejs && rm -rf /var/lib/apt/lists/* \
    && npm install --global --no-audit --no-fund pnpm@${PNPM_VERSION} \
       @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
       @openai/codex@${CODEX_VERSION}

# Operator-selected observation tools, pinned for linux/arm64. They carry
# no credentials. A role receives only its explicitly configured access.
RUN { test "$(uname -m)" = "aarch64" || { echo "deploy/pod/Dockerfile builds linux/arm64 only (kubectl and the AWS CLI are pinned for arm64)" >&2; exit 1; }; } \
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
    && apt-get purge -y -qq unzip && apt-get autoremove -y -qq \
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

# The same Go toolchain is available to configured validation commands.
COPY --from=bundle /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH

# Rust for configured validation commands, pinned like the tools above: the
# standalone components of one stable release (static.rust-lang.org's
# channel-rust-stable.toml of RUST_DIST_DATE names each file and its sha256),
# installed under /usr/local. A role runs with a cleared environment, so this
# is not rustup: cargo and rustc are the real programs on the role's PATH and
# need no RUSTUP_HOME or CARGO_HOME; cargo writes only under the role's HOME
# (or the CARGO_HOME and CARGO_TARGET_DIR a command sets). gcc and libc6-dev
# are the linker rustc calls. clippy and rustfmt are there for a role that runs
# them; no check of this image requires them.
ARG RUST_VERSION=1.99.0
ARG RUST_DIST_DATE=2026-10-01
ARG RUSTC_SHA256=89c0f1a3a44df63c95e5d5f2ba093d0a12c5f5f57f8635f8a7a0ce50cf86600e
ARG RUST_STD_SHA256=1cf7e2ef58ed1cfa6f1adea18e155cbf254c1951f39e5f5e3376879f55fd64a4
ARG CARGO_SHA256=113229fcc16cc1ba004923a4cc275fbf3281dafaaf8b97269f9c00189d497fd4
ARG CLIPPY_SHA256=42e49dc02d29949395c0c072585961f6c9b5408110236087828d4e2f94ca96ef
ARG RUSTFMT_SHA256=4b9162fef99fd5246d2f83a2430c144e9bb9095aac6d1de0aaebde45ec01e09a
RUN { test "$(uname -m)" = "aarch64" || { echo "deploy/pod/Dockerfile builds linux/arm64 only (the Rust components are pinned for aarch64)" >&2; exit 1; }; } \
    && apt-get update -qq && apt-get install -y -q --no-install-recommends gcc libc6-dev xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir /tmp/rust-install && cd /tmp/rust-install \
    && for component in "rustc ${RUSTC_SHA256}" "rust-std ${RUST_STD_SHA256}" "cargo ${CARGO_SHA256}" \
                        "clippy ${CLIPPY_SHA256}" "rustfmt ${RUSTFMT_SHA256}"; do \
         name="${component% *}" && sum="${component#* }" \
         && file="${name}-${RUST_VERSION}-aarch64-unknown-linux-gnu.tar.xz" \
         && curl -fsSL -o "$file" "https://static.rust-lang.org/dist/${RUST_DIST_DATE}/${file}" \
         && echo "${sum}  ${file}" | sha256sum -c - \
         && tar -xJf "$file" \
         && "./${file%.tar.xz}/install.sh" --prefix=/usr/local --disable-ldconfig >/dev/null \
         || exit 1; \
       done \
    && cd / && rm -rf /tmp/rust-install /usr/local/share/doc/rust \
    # Debian's /usr/bin/cc is a symlink through /etc/alternatives; a role sees
    # /usr but only named files of /etc, so cc has to resolve inside /usr for
    # rustc to find its linker there (check-rust-toolchain.sh holds it to that).
    && ln -sfn gcc /usr/bin/cc \
    && test "$(readlink /usr/bin/cc)" = gcc \
    && cc --version | head -n 1 \
    && rustc --version && cargo --version && cargo clippy --version && rustfmt --version \
    && test "$(rustc --version | cut -d' ' -f2)" = "${RUST_VERSION}"

# The role-chain bundle, its fixed delivery processes and the role syscall
# policy. All three stay outside every model-writable directory: a role
# receives them as explicit read-only runtime paths (rewrite/RUNTIME.md,
# "Linux role launch"), and the operator's configuration decides which role
# may run which of them.
COPY --from=bundle /out/bundle /opt/ticket-automation/bundle
COPY rewrite/harnesses/delivery_support.py rewrite/harnesses/deliver_git.py \
     rewrite/harnesses/verify_merged.py rewrite/harnesses/mirror_loop.py \
     /opt/ticket-automation/scripts/
COPY rewrite/runtime-policy/compile_role_seccomp.py \
     rewrite/runtime-policy/role_bwrap_policy.py \
     /opt/ticket-automation/runtime-policy/
# The pinned profile is fetched and checked here, like the tools above, so
# the diagnostic copy used during measurement never becomes the source of
# truth. The last line runs the packaged engine: a bundle built for another
# machine cannot answer, so the build fails here instead of in the cluster.
RUN curl -fsSL -o /opt/ticket-automation/runtime-policy/moby-seccomp.json \
      "https://raw.githubusercontent.com/moby/profiles/${MOBY_SECCOMP_REF}/seccomp/default.json" \
    && echo "${MOBY_SECCOMP_SHA256}  /opt/ticket-automation/runtime-policy/moby-seccomp.json" | sha256sum -c - \
    && python3 -B -c 'import json,sys; json.load(open(sys.argv[1]))' /opt/ticket-automation/runtime-policy/moby-seccomp.json \
    && chmod 0755 /opt/ticket-automation/runtime-policy/role_bwrap_policy.py \
    && /opt/ticket-automation/bundle/bin/ticket-engine --help 2>&1 | grep -q 'run-dir'

# Non-root default; installation manifests choose their explicit runtime policy.
RUN useradd --create-home --uid 1000 lassdas
USER lassdas
WORKDIR /home/lassdas

ENTRYPOINT ["/opt/ticket-automation/bundle/bin/ticket-engine"]
