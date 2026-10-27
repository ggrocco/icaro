# Ícaro agent base image (architecture §5).
#
# - non-root user `icaro` (uid 1000); sessions run as the host uid:gid and
#   HOME=/home/icaro is the profile home mounted by the daemon
# - asdf for per-profile runtimes, materialized at setup
# - optional agent CLIs (Claude Code, Codex) via npm
# - the static icaro binary for internal commands (`icaro mcp --socket`,
#   `icaro egress-proxy`)
# - never any credentials
#
# Build from the repository root:
#   make image-agent            # uses bin/icaro-linux (CGO_ENABLED=0)
#
# Every input is pinned: the base image by digest (Dependabot bumps it),
# Node.js by version and SHA-256, the agent CLIs by exact npm version.
# To move one by hand:
#   base   docker buildx imagetools inspect debian:trixie-slim  (the index digest)
#   node   https://nodejs.org/dist/<version>/SHASUMS256.txt     (linux-x64 / linux-arm64 .tar.xz)
#   agents npm view @anthropic-ai/claude-code version; npm view @openai/codex version
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

# asdf ships one static binary per architecture; the release only publishes
# md5 files, so the SHA-256 of each tarball is pinned here.
ARG ASDF_VERSION=v0.20.2
ARG ASDF_SHA256_AMD64=6d45e6fab12f50a259b5643adf06a2e77af30a87300b4402440eda09af467a6d
ARG ASDF_SHA256_ARM64=356e135a608bdfba34038c1f18c4b82434d7b6d546e30dbf372c61baaa2f442c
ARG NODE_VERSION=v24.22.1
ARG NODE_SHA256_AMD64=6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6
ARG NODE_SHA256_ARM64=dbabf7a1cbf947e6059ffad2c27b80041db97dea1e553b62afdee7f13fca336e
ARG CLAUDE_CODE_VERSION=2.1.310
ARG CODEX_VERSION=0.164.0
# Exact name@version specs; empty installs no agent CLI.
ARG INSTALL_AGENTS="@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} @openai/codex@${CODEX_VERSION}"

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    ASDF_DATA_DIR=/opt/asdf-data \
    PATH=/opt/asdf-data/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git jq less openssh-client procps ripgrep tini unzip xz-utils zsh make gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

# asdf (system-wide, read-only at runtime; plugins/runtimes are added by setup).
# TARGETARCH is set by BuildKit; the legacy builder falls back to dpkg.
ARG TARGETARCH
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && case "${arch}" in \
      amd64) sum="${ASDF_SHA256_AMD64}" ;; \
      arm64) sum="${ASDF_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: ${arch}" >&2; exit 1 ;; \
    esac \
 && curl -fsSLo /tmp/asdf.tar.gz "https://github.com/asdf-vm/asdf/releases/download/${ASDF_VERSION}/asdf-${ASDF_VERSION}-linux-${arch}.tar.gz" \
 && echo "${sum}  /tmp/asdf.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/asdf.tar.gz -C /usr/local/bin asdf \
 && rm /tmp/asdf.tar.gz \
 && mkdir -p "${ASDF_DATA_DIR}" && chmod 0755 "${ASDF_DATA_DIR}"

# Node.js for npm-distributed agent CLIs, from the official tarball checked
# against its pinned SHA-256 (no piped install script, no extra apt repo).
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && case "${arch}" in \
      amd64) narch=x64; sum="${NODE_SHA256_AMD64}" ;; \
      arm64) narch=arm64; sum="${NODE_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: ${arch}" >&2; exit 1 ;; \
    esac \
 && curl -fsSLo /tmp/node.tar.xz "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-${narch}.tar.xz" \
 && echo "${sum}  /tmp/node.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner \
 && rm /tmp/node.tar.xz /usr/local/CHANGELOG.md /usr/local/README.md \
 && mkdir -p /usr/local/share/doc/node && mv /usr/local/LICENSE /usr/local/share/doc/node/ \
 && node --version && npm --version

# The agent CLIs at exact versions. npm 11 only runs the install scripts of
# allowed packages; allow the agent packages themselves (by bare name, the
# @version stripped) but none of their dependencies.
RUN if [ -n "${INSTALL_AGENTS}" ]; then \
      allow="$(for spec in ${INSTALL_AGENTS}; do \
          case "${spec#?}" in *@*) echo "${spec%@*}" ;; *) echo "${spec}" ;; esac; \
        done | paste -sd, -)" \
      && npm install -g --allow-scripts="${allow}" ${INSTALL_AGENTS} \
      && npm cache clean --force; \
    fi

RUN useradd --create-home --uid 1000 --shell /bin/bash icaro

# Multi-arch builds (buildx) provide bin/icaro-linux-<arch>; a local
# `make image-agent` provides bin/icaro-linux.
COPY bin/icaro-linux* /tmp/icaro/
RUN if [ -f "/tmp/icaro/icaro-linux-${TARGETARCH}" ]; then cp "/tmp/icaro/icaro-linux-${TARGETARCH}" /usr/local/bin/icaro; \
    else cp /tmp/icaro/icaro-linux /usr/local/bin/icaro; fi \
 && chmod 0755 /usr/local/bin/icaro && rm -rf /tmp/icaro

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash", "-l"]
