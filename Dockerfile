# syntax=docker/dockerfile:1.7

# ---------- go builder ----------
FROM golang:1.25-alpine AS go-builder

# CGO deps for mattn/go-sqlite3
RUN apk add --no-cache gcc musl-dev sqlite-dev git

WORKDIR /src

# Cache module downloads first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# PROTO_VERSION is stamped into the binary and reported by /v1/proxy-info
# and `cursor-proxy -version`. CI passes the git tag (e.g. cursor3.11/v0.2.1);
# `docker build` without --build-arg keeps the "dev" default.
ARG PROTO_VERSION=dev

# Build only the cursor-proxy binary
# CGO_ENABLED=1 because mattn/go-sqlite3 needs libc
ENV CGO_ENABLED=1 \
    GOOS=linux
RUN go build -trimpath \
    -ldflags="-s -w -X main.ProtoVersion=${PROTO_VERSION}" \
    -o /out/cursor-proxy \
    ./cmd/cursor-proxy

# ---------- node runner builder ----------
# Separate stage: install @cursor/sdk + compile TypeScript. We copy
# the compiled JS + minimal production node_modules into the final
# image; the TS source, dev deps, and build cache stay behind.
FROM node:22-alpine AS node-builder

# @cursor/sdk pulls in optional native subprocess bindings; the alpine
# image is missing python and make, so builds fail loudly without
# these. Kept minimal — no C++ compiler needed for pure JS install.
RUN apk add --no-cache python3 make g++

WORKDIR /src/node-runner

# Cache npm install
COPY node-runner/package.json node-runner/package-lock.json ./
RUN npm ci --no-audit --no-fund

# Compile TypeScript
COPY node-runner/tsconfig.json ./
COPY node-runner/src ./src
RUN npx tsc

# Prune dev dependencies for the runtime image. --omit=dev leaves us
# with only @cursor/sdk + its runtime deps; no tsx, no typescript, no
# @types.
RUN npm prune --production --no-audit --no-fund

# ---------- runtime ----------
FROM alpine:3.20

# Base runtime deps + Node.js (~40 MB). We install Node from apk
# rather than copying from node-builder so we get glibc-compatible
# musl builds that alpine actually runs.
RUN apk add --no-cache ca-certificates sqlite-libs tzdata nodejs \
 && addgroup -g 1000 -S cursor \
 && adduser  -u 1000 -S cursor -G cursor \
 && mkdir -p /data/accounts \
 && chown -R 1000:1000 /data

# Go binary
COPY --from=go-builder /out/cursor-proxy /usr/local/bin/cursor-proxy

# Node runner. dist/ has the compiled JS; node_modules/ carries only
# @cursor/sdk + its transitive runtime deps (dev deps pruned above).
# We install into /opt/cursor-node-runner and pass its path via env
# so users don't need to know the layout.
COPY --from=node-builder /src/node-runner/dist         /opt/cursor-node-runner/dist
COPY --from=node-builder /src/node-runner/node_modules /opt/cursor-node-runner/node_modules
COPY --from=node-builder /src/node-runner/package.json /opt/cursor-node-runner/package.json

# Agent mode auto-detection: -node-runner defaults to the bundled
# path so operators only need to set CURSOR_API_KEY to enable it.
# Leaving CURSOR_API_KEY empty keeps agent mode off — wire mode
# is untouched.
ENV CURSOR_PROXY_NODE_RUNNER=/opt/cursor-node-runner/dist/index.js

USER 1000
WORKDIR /data

EXPOSE 8317

ENTRYPOINT ["cursor-proxy", "-addr", "0.0.0.0:8317"]
