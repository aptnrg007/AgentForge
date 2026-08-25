# syntax=docker/dockerfile:1

# --- build: static agentforge binary, no cgo (see README's "Building") ---
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agentforge ./cmd/agentforge

# --- runtime: agentforge plus everything the examples/ configs shell out to ---
#
# Every tool_definitions/mcp dependency across examples/*.yaml, bundled so a
# user never has to install any of it on the host:
#   - nodejs/npm (npx)  -> @modelcontextprotocol/server-{filesystem,memory,github,everything}
#   - uv (uvx)          -> mcp-server-fetch (weather.yaml, article-digest.yaml)
#   - git, ripgrep       -> repo-assistant.yaml, diff-reviewer.yaml
#   - sqlite (sqlite3)  -> sql-analyst.yaml
# grep/sed (log-investigator.yaml) come from busybox, already in the base image.
FROM alpine:3.20

RUN apk add --no-cache \
      ca-certificates \
      nodejs npm \
      git \
      ripgrep \
      sqlite \
      curl

# uv's installer detects musl vs glibc itself, so this works unmodified on
# Alpine — see https://github.com/astral-sh/uv#installation.
RUN curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh \
    && apk del curl

# Pre-installed globally so npx resolves them from disk instead of the
# registry on every run — faster, and works without network once built. No
# config changes needed: `npx -y <pkg>` finds an already-installed package
# before it ever considers fetching one.
RUN npm install -g \
      @modelcontextprotocol/server-filesystem \
      @modelcontextprotocol/server-memory \
      @modelcontextprotocol/server-github \
      @modelcontextprotocol/server-everything \
    && npm cache clean --force

RUN addgroup -S agentforge && adduser -S agentforge -G agentforge
COPY --from=builder /out/agentforge /usr/local/bin/agentforge

# Pre-create the store directory owned by the non-root user before
# declaring it a volume — otherwise Docker materializes an anonymous or
# named volume there owned by root, and the container can't write
# ~/.agentforge/agentforge.db.
RUN mkdir -p /home/agentforge/.agentforge && chown agentforge:agentforge /home/agentforge/.agentforge

USER agentforge
WORKDIR /home/agentforge
# So `agentforge run examples/...` works exactly as documented in README.md
# with no path changes. See README's "Docker" section for mounting your own
# configs or a live repo instead of this baked-in copy.
COPY --chown=agentforge:agentforge examples ./examples

VOLUME ["/home/agentforge/.agentforge"]

ENTRYPOINT ["agentforge"]
