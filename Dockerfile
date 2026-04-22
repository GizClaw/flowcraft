# Multi-stage build for FlowCraft runtime image.
# Produces a minimal Linux container with the full-featured server binary.

# --- Stage 1: Build ---
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev git nodejs npm

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

WORKDIR /src/web
RUN npm ci && npm run build

WORKDIR /src
RUN CGO_ENABLED=1 GOOS=linux go build -tags linux \
    -ldflags="-s -w" \
    -o /usr/local/bin/flowcraft \
    ./cmd/flowcraft

# --- Stage 2: Runtime ---
FROM alpine:3.21

RUN apk add --no-cache \
    bubblewrap \
    ca-certificates \
    sqlite \
    tini

COPY --from=builder /usr/local/bin/flowcraft /usr/local/bin/flowcraft
COPY --from=builder /src/web/dist /opt/flowcraft/web/dist

ENV FLOWCRAFT_WEB_DIR=/opt/flowcraft/web/dist

EXPOSE 8080

VOLUME ["/data"]

ENTRYPOINT ["tini", "--"]
CMD ["flowcraft", "server"]
