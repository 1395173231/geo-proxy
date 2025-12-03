# Build Stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 安装 git 和 ca-certificates (下载数据库需要 https)
RUN apk add --no-cache git ca-certificates

# 预先下载依赖，利用 Docker 缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 编译为静态二进制文件
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o geo-proxy main.go

# Final Stage
FROM alpine:latest

# 安装 CA 证书以支持 HTTPS 请求 (下载数据库用)
RUN apk --no-cache add ca-certificates

WORKDIR /root/

# 从构建层复制二进制文件
COPY --from=builder /app/geo-proxy .

# 创建数据目录
RUN mkdir -p data

# 设置环境变量默认值
ENV PORT=8080
ENV BACKEND_URL="http://host.docker.internal:8000"

EXPOSE 8080

CMD ["./geo-proxy"]