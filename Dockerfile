# syntax=docker/dockerfile:1.6
# ---- Stage 1: build the static Go binary ----
FROM golang:1.26.4-alpine AS build

WORKDIR /src

# Cache go.mod/go.sum first to avoid re-fetching deps on code-only changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY . .

# Build a fully static binary (CGO disabled) for linux/amd64.
# Trim path and strip debug info to keep the image small.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/bot ./cmd/bot

# Build a small static HTTP probe so the distroless runtime
# image (which has no shell, no curl, no wget) can run Docker
# HEALTHCHECK. Probe logic: http.Get URL; exit 0 on 2xx/3xx,
# 1 otherwise. The probe is 2MB vs the multi-MB wget alternative.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/healthprobe ./cmd/healthprobe

# ---- Stage 2: minimal runtime image ----
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

LABEL org.opencontainers.image.title="narrative-ai-agent" \
      org.opencontainers.image.description="Multi-transport narrative RPG bot" \
      org.opencontainers.image.source="https://github.com/bestxp/narrative-ai-agent"

WORKDIR /app

# Bot binary + health probe.
COPY --from=build /out/bot         /app/bot
COPY --from=build /out/healthprobe /app/healthprobe

# k8s livenessProbe / readinessProbe / Docker healthcheck target.
# The bot's health server (stdlib net/http) listens on this port;
# default 8080 is overridden via config.yaml (health.listen_addr).
EXPOSE 8080

# Run as the unprivileged "nonroot" user (UID 65532).
USER nonroot:nonroot

ENTRYPOINT ["/app/bot"]
