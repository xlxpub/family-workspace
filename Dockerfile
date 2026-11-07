# syntax=docker/dockerfile:1
# 多阶段构建：纯 Go 驱动免 CGO，产物为单个静态二进制（前端由 embed.FS 内嵌）
FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# BuildKit 自动注入 TARGETOS/TARGETARCH：在 ARM NAS 上构建会自动产出 arm64 镜像
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/family-server .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -S -g 10001 fw \
 && adduser -S -u 10001 -G fw fw \
 && mkdir -p /data \
 && chown -R fw:fw /data
WORKDIR /app
COPY --from=builder /out/family-server /app/family-server
USER fw
ENV FW_DB=/data/family.db \
    FW_ADDR=:8080 \
    TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8080
# /api/v1/health 在鉴权白名单内，无需凭证即可探活
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/v1/health >/dev/null || exit 1
CMD ["/app/family-server"]
