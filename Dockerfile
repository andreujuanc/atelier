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
# claude, git, docker CLI/compose, Node.js LTS with npm and corepack, and
# everyday tools. No other language: a project brings its own in its image,
# or a task installs it. No atl (it is mounted into task containers) and no
# tmux.
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

# The default task image: lean on purpose. Languages other than Node.js come
# from a project's own image (e.g. FROM atelier-task) or are installed in the
# task.
FROM base AS task
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
        jq ripgrep unzip zip xz-utils procps \
        libatomic1 \
    && rm -rf /var/lib/apt/lists/*

# Node.js (the newest release of the current LTS line) from nodejs.org, with
# npm, checked against the release's SHASUMS256.txt, into /usr/local (it
# needs libatomic1, above), without its C headers (only native addons need
# them, and those need a compiler and python3 too, which this image has not).
# Not NodeSource's package: it depends on python3. corepack (no longer shipped
# with Node.js) from npm, its yarn and pnpm shims enabled.
ARG NODE_MAJOR=26
RUN arch="$(dpkg --print-architecture)" \
    && case "$arch" in amd64) arch=x64 ;; arm64) arch=arm64 ;; *) echo "no Node.js build for $arch" >&2; exit 1 ;; esac \
    && url="https://nodejs.org/dist/latest-v$NODE_MAJOR.x" \
    && curl -fsSL "$url/SHASUMS256.txt" -o /tmp/SHASUMS256.txt \
    && file="$(grep -oE "node-v$NODE_MAJOR\.[0-9.]+-linux-$arch\.tar\.xz$" /tmp/SHASUMS256.txt)" \
    && curl -fsSL "$url/$file" -o "/tmp/$file" \
    && (cd /tmp && grep "  $file\$" SHASUMS256.txt | sha256sum -c -) \
    && tar -xJf "/tmp/$file" -C /usr/local --strip-components=1 --no-same-owner \
        --anchored --no-wildcards-match-slash \
        --exclude='node-v*/include' --exclude='node-v*/CHANGELOG.md' --exclude='node-v*/README.md' \
    && mkdir -p /usr/local/share/doc/node && mv /usr/local/LICENSE /usr/local/share/doc/node/ \
    && rm "/tmp/$file" /tmp/SHASUMS256.txt \
    && (command -v corepack > /dev/null || npm install -g corepack) \
    && corepack enable \
    && npm cache clean --force

# The ordinary user's npm install -g goes into ~/.local (its bin is on PATH
# already, for Claude Code). corepack fetches pnpm or yarn without asking
# first (no one may be there to answer).
ARG USERNAME=atelier
ENV NPM_CONFIG_PREFIX=/home/$USERNAME/.local \
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
