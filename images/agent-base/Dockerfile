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
ARG DEBIAN_VERSION=bookworm-slim
FROM debian:${DEBIAN_VERSION}

ARG ASDF_VERSION=v0.14.1
ARG NODE_MAJOR=22
ARG INSTALL_AGENTS="@anthropic-ai/claude-code @openai/codex"

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    ASDF_DIR=/opt/asdf \
    ASDF_DATA_DIR=/opt/asdf-data \
    PATH=/opt/asdf/bin:/opt/asdf-data/shims:/usr/local/bin:/usr/bin:/bin

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git jq less openssh-client procps ripgrep tini unzip xz-utils zsh make gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

# asdf (system-wide, read-only at runtime; plugins/runtimes are added by setup)
RUN git clone --depth 1 --branch "${ASDF_VERSION}" https://github.com/asdf-vm/asdf.git "${ASDF_DIR}" \
 && mkdir -p "${ASDF_DATA_DIR}" && chmod 0755 "${ASDF_DATA_DIR}"

# Node.js for npm-distributed agent CLIs.
RUN curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && if [ -n "${INSTALL_AGENTS}" ]; then npm install -g ${INSTALL_AGENTS} && npm cache clean --force; fi

RUN useradd --create-home --uid 1000 --shell /bin/bash icaro

COPY bin/icaro-linux /usr/local/bin/icaro

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash", "-l"]
