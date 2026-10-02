#
# ClassWatch Control Plane API image.
#
# Two binaries are produced from this one module: the API server and the
# migration runner. Shipping them in the same image guarantees the migrations a
# deploy applies are exactly the ones compiled into the API that follows —
# version skew between "the schema tool" and "the server" is a classic source of
# half-applied releases.
#
# Build context is services/api, so:  docker build -t classwatch-api:phase0 services/api
#
# 故意不写 `# syntax=docker/dockerfile:1`：本文件只用标准多阶段语法，
# 而引入外部 syntax 前镜像会让构建多依赖一次 Docker Hub 拉取（离线/弱网会直接失败）。

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

# The build arguments become the version metadata reported by GET /api/v1/meta
# and printed at startup. They default to honest placeholders rather than to a
# fake release number: an unstamped image must be identifiable as such.
ARG VERSION=dev
ARG COMMIT=unknown

# git is not needed (no private modules, no VCS stamping) and CGO is off, so the
# build stays fully static and the runtime stage needs no libc.
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS=-trimpath

WORKDIR /src

# Dependencies first: this layer is cached until go.mod/go.sum change, so editing
# application code does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -s -w drop the symbol table and DWARF data (smaller image, no source paths);
# -X injects the build identity. Static binaries keep the runtime image minimal.
RUN go build -ldflags="-s -w \
      -X github.com/classwatch/classwatch/services/api/internal/httpapi.Version=${VERSION} \
      -X github.com/classwatch/classwatch/services/api/internal/httpapi.Commit=${COMMIT}" \
      -o /out/api ./cmd/api \
 && go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM alpine:3.22

# ca-certificates: outgoing TLS to LiveKit and to PostgreSQL over TLS.
# tzdata: timestamps must be renderable in the deployment's local time.
# wget: the HEALTHCHECK below, and busybox wget is the smallest way to get it.
RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -S -g 10001 classwatch \
 && adduser -S -u 10001 -G classwatch -H -s /sbin/nologin classwatch

WORKDIR /app

COPY --from=build /out/api /app/api
COPY --from=build /out/migrate /app/migrate

# Never run as root: the API talks to two data stores and terminates untrusted
# input, so a container escape should not start from uid 0.
USER 10001:10001

EXPOSE 8080

# Liveness only. /readyz is deliberately NOT used here: an orchestrator restarting
# the container because PostgreSQL is briefly unreachable turns a dependency blip
# into an outage. Readiness belongs to the platform's own probe.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

# Secrets arrive as environment variables at runtime; nothing secret is baked into
# a layer. LIVEKIT_API_SECRET in particular must never appear in the image or in
# the build cache.
ENTRYPOINT ["/app/api"]
