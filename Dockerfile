FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /mctl-agent ./cmd/agent

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

RUN addgroup -g 1000 app && adduser -D -u 1000 -G app app

COPY --from=builder /mctl-agent /usr/local/bin/mctl-agent

RUN mkdir -p /data && chown app:app /data

USER app:app

ENTRYPOINT ["mctl-agent"]
