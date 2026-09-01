# Digest resolved 2026-09-01 via Docker Registry HTTP API v2 manifest list
# lookup against registry-1.docker.io for golang:1.26.6-alpine (matches the
# go 1.26.6 directive in go.mod).
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /mctl-agent ./cmd/agent

FROM alpine:3.24

RUN apk add --no-cache ca-certificates

RUN addgroup -g 1000 app && adduser -D -u 1000 -G app app

WORKDIR /app

COPY --from=builder /mctl-agent /usr/local/bin/mctl-agent

# cmd/agent/main.go loads YAML skills from the relative path "skills/custom",
# resolved against the process cwd (this WORKDIR) at runtime.
COPY --from=builder --chown=app:app /app/skills/custom ./skills/custom

RUN mkdir -p /data && chown app:app /data

USER app:app

ENTRYPOINT ["mctl-agent"]
