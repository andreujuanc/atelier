# atelier's two images, built from one file. Build them with your host user so
# that paths inside the containers match the host (git worktrees store
# absolute paths):
#
# The box (the last stage, so the default target): atl + claude + tmux + git +
# gh + docker CLI/compose, where atl itself runs.
#
#   docker build -t atelier \
#     --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) .
#
# The default task image (ATL_SANDBOX=docker): what a task's sessions run in.
# claude, git, docker CLI/compose, a C toolchain and make, Node.js LTS with npm
# and corepack, Python 3 with pip and venv, Go, and everyday tools. No atl
# (it is mounted into task containers) and no tmux.
#
#   docker build --target task -t atelier-task \
#     --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) .
#
# Docker's legacy builder (DOCKER_BUILDKIT=0) builds every stage up to the
# target, so building the box builds the task stage too (cached afterwards);
# BuildKit skips it.

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/atl ./cmd/atl

# What both images have: git, ssh client, the Docker CLI, the user and
# Claude Code.
FROM debian:trixie-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git less openssh-client gnupg \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + compose plugin (talks to the host daemon, or a task's gate,
# through a socket; no daemon here).
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && echo "deb [signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

# UTF-8 everywhere, otherwise tmux replaces Claude's logo and box characters.
ENV LANG=C.UTF-8

ARG USERNAME=atelier
ARG UID=1000
ARG GID=1000
RUN (getent group "$GID" || groupadd -g "$GID" "$USERNAME") \
    && useradd -m -u "$UID" -g "$GID" -s /bin/bash "$USERNAME"

USER $USERNAME
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH=/home/$USERNAME/.local/bin:$PATH
WORKDIR /home/$USERNAME

# The default task image.
FROM base AS task
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential pkg-config \
        python3 python3-pip python3-venv python-is-python3 \
        jq ripgrep unzip zip xz-utils procps \
    && rm -rf /var/lib/apt/lists/*

# Node.js (the current LTS line) from NodeSource, with npm; corepack (no
# longer shipped with Node.js) from npm, its yarn and pnpm shims enabled.
ARG NODE_MAJOR=26
RUN curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key -o /etc/apt/keyrings/nodesource.asc \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.asc] https://deb.nodesource.com/node_$NODE_MAJOR.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && (command -v corepack > /dev/null || npm install -g corepack) \
    && corepack enable \
    && npm cache clean --force

# Go from go.dev.
ARG GO_VERSION=1.27.4
RUN arch="$(dpkg --print-architecture)" \
    && curl -fsSL "https://dl.google.com/go/go$GO_VERSION.linux-$arch.tar.gz" -o /tmp/go.tgz \
    && echo "$(curl -fsSL "https://dl.google.com/go/go$GO_VERSION.linux-$arch.tar.gz.sha256")  /tmp/go.tgz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz \
    && echo 'export PATH="$PATH:$HOME/go/bin:/usr/local/go/bin"' > /etc/profile.d/go.sh

# The ordinary user installs into their home, all on PATH: npm install -g
# into ~/.local, pip install (--user) into ~/.local (Debian's Python refuses
# without the override; a venv needs none), go install into ~/go/bin.
# corepack fetches pnpm or yarn without asking first (no one may be there
# to answer).
ARG USERNAME=atelier
ENV PATH=$PATH:/home/$USERNAME/go/bin:/usr/local/go/bin \
    NPM_CONFIG_PREFIX=/home/$USERNAME/.local \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0
USER $USERNAME
CMD ["bash", "-l"]

# The box: atl, tmux, gh.
FROM base AS box
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
        tmux gh ncurses-term \
    && rm -rf /var/lib/apt/lists/*

# Keep tmux out of the way: no status bar, mouse scrolling, Ctrl-q returns to atl.
RUN printf '%s\n' \
        'set -g status off' \
        'set -g mouse on' \
        'set -g escape-time 10' \
        'bind -n C-q detach-client' \
        'set -g default-terminal "tmux-256color"' \
        'set -as terminal-features ",*:RGB"' \
        > /etc/tmux.conf

COPY --from=build /out/atl /usr/local/bin/atl

ARG USERNAME=atelier
USER $USERNAME
RUN mkdir -p "$HOME/.local/share/atelier" "$HOME/.local/state/atelier"
ENV CLAUDE_CONFIG_DIR=/home/$USERNAME/.claude
# A plain shell, not tmux: atl must run outside tmux so opening a task
# attaches (not switch-client) and Ctrl-q's detach returns to the TUI
# instead of detaching the container's main tmux client and exiting.
CMD ["bash", "-l"]
