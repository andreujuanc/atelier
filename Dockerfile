# atelier box: atl + claude + tmux + git + gh + docker CLI/compose in one image.
#
# Build with your host user so paths inside the container match the host
# (git worktrees store absolute paths):
#
#   docker build -t atelier \
#     --build-arg USERNAME=$(whoami) --build-arg UID=$(id -u) --build-arg GID=$(id -g) .

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/atl ./cmd/atl

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git tmux gh less openssh-client ncurses-term gnupg \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + compose plugin (talks to the host daemon through a mounted socket; no daemon here).
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && echo "deb [signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
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

# UTF-8 everywhere, otherwise tmux replaces Claude's logo and box characters.
ENV LANG=C.UTF-8

ARG USERNAME=atelier
ARG UID=1000
ARG GID=1000
RUN (getent group "$GID" || groupadd -g "$GID" "$USERNAME") \
    && useradd -m -u "$UID" -g "$GID" -s /bin/bash "$USERNAME"

COPY --from=build /out/atl /usr/local/bin/atl

USER $USERNAME
RUN curl -fsSL https://claude.ai/install.sh | bash \
    && mkdir -p "$HOME/.local/share/atelier" "$HOME/.local/state/atelier"
ENV PATH=/home/$USERNAME/.local/bin:$PATH \
    CLAUDE_CONFIG_DIR=/home/$USERNAME/.claude
WORKDIR /home/$USERNAME
# A plain shell, not tmux: atl must run outside tmux so opening a task
# attaches (not switch-client) and Ctrl-q's detach returns to the TUI
# instead of detaching the container's main tmux client and exiting.
CMD ["bash", "-l"]
