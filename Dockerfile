# syntax=docker/dockerfile:1.7

# ---------- builder ----------
FROM golang:1.25-alpine AS builder

# CGO deps for mattn/go-sqlite3
RUN apk add --no-cache gcc musl-dev sqlite-dev git

WORKDIR /src

# Cache module downloads first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build only the cursor-proxy binary
# CGO_ENABLED=1 because mattn/go-sqlite3 needs libc
ENV CGO_ENABLED=1 \
    GOOS=linux
RUN go build -trimpath -ldflags="-s -w" \
    -o /out/cursor-proxy \
    ./cmd/cursor-proxy

# ---------- runtime ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates sqlite-libs tzdata \
 && addgroup -g 1000 -S cursor \
 && adduser  -u 1000 -S cursor -G cursor \
 && mkdir -p /data/accounts \
 && chown -R 1000:1000 /data

COPY --from=builder /out/cursor-proxy /usr/local/bin/cursor-proxy

USER 1000
WORKDIR /data

EXPOSE 8317

ENTRYPOINT ["cursor-proxy", "-addr", "0.0.0.0:8317"]
