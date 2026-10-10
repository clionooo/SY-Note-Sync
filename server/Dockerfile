# SY Note Sync 服务端镜像
# 两阶段构建：第一阶段编译 Go 二进制，第二阶段只放二进制（镜像约 20MB）
FROM golang:1.22-alpine AS builder
WORKDIR /build
# 国内网络可加代理：ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/syncsrv .

FROM alpine:3.19
# ca-certificates：HTTPS 反代/WSS 需要
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=builder /out/syncsrv /app/syncsrv
# 数据目录（挂载卷持久化 sqlite 与快照）
VOLUME /app/data
EXPOSE 58282
ENTRYPOINT ["/app/syncsrv"]
CMD ["-addr", ":58282", "-db", "/app/data/sync.db", "-data", "/app/data"]
