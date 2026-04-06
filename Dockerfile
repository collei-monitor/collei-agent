# ---- Builder ----
FROM golang:1.25-alpine AS builder

ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/collei-monitor/collei-agent/internal/core.Version=${VERSION}" \
    -o /collei-agent .

# ---- Runtime ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 -S collei \
    && adduser  -u 10001 -S -G collei -h /nonexistent -s /sbin/nologin collei \
    && mkdir -p /etc/collei-agent \
    && chown collei:collei /etc/collei-agent

COPY --from=builder /collei-agent /usr/local/bin/collei-agent

USER collei

ENTRYPOINT ["collei-agent", "run", "--no-auto-update", "--config", "/etc/collei-agent/agent.yaml"]
