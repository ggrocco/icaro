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
ARG DEBIAN_VERSION=trixie-slim
FROM debian:${DEBIAN_VERSION}

# asdf ships one static binary per architecture; the release only publishes
# md5 files, so the SHA-256 of each tarball is pinned here.
ARG ASDF_VERSION=v0.20.2
ARG ASDF_SHA256_AMD64=6d45e6fab12f50a259b5643adf06a2e77af30a87300b4402440eda09af467a6d
ARG ASDF_SHA256_ARM64=356e135a608bdfba34038c1f18c4b82434d7b6d546e30dbf372c61baaa2f442c
ARG NODE_MAJOR=24
ARG INSTALL_AGENTS="@anthropic-ai/claude-code @openai/codex"

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

# Node.js for npm-distributed agent CLIs. npm 11 only runs the install
# scripts of allowed packages; allow the agent packages themselves (by bare
# name) but none of their dependencies.
RUN curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && if [ -n "${INSTALL_AGENTS}" ]; then \
      npm install -g --allow-scripts="$(echo ${INSTALL_AGENTS} | tr ' ' ',')" ${INSTALL_AGENTS} && npm cache clean --force; \
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
