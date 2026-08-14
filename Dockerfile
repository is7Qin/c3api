# github.com/is7qin/c3api 多阶段构建：前端 dist 内嵌进 Go 二进制（cmd/server/embed.go），
# 运行时为单二进制 + 挂载 config。
# 用法：docker compose -f deploy/compose.yml up -d --build

# ---- Stage 1: 前端构建（vite build → dist） ----
FROM node:24-alpine AS web
RUN corepack enable
WORKDIR /web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
# frozen-lockfile 保证可复现；hoisted linker 与本地开发一致（Linux 容器内无 junction 问题，保持统一）
RUN pnpm install --config.node-linker=hoisted --frozen-lockfile
COPY web/ .
RUN pnpm build

# ---- Stage 2: Go 构建（前端 dist 复制进 embed 目录） ----
# C3API_VERSION（默认 "dev"）：版本注入点——-ldflags -X 把版本写进二进制 version
# 变量（cmd/server/main.go 的 -version 输出，REL spec 2026-08-15）。release 流水线
# （.github/workflows/release.yml）构建镜像时经 build-args 传 tag 值（如
# v0.0.1-beta.1）；本地 dev 构建不传 ARG = 默认 "dev"（dev 构建自报版本不被置空——
# 若无默认值，空 ARG 会注入 "-X main.version=" 空串，语义漂移）。
FROM golang:1.26-alpine AS gobuild
ARG C3API_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 多阶段产物经 COPY --from=web 传递（阶段间是独立容器，不能直接 cp 前阶段路径）
RUN rm -rf cmd/server/dist && mkdir -p cmd/server/dist
COPY --from=web /web/dist ./cmd/server/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${C3API_VERSION}" -o /out/server ./cmd/server

# ---- Stage 3: 运行时（精简 alpine，非 root） ----
FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=gobuild /out/server /app/server
WORKDIR /app
EXPOSE 18080
ENTRYPOINT ["/app/server"]
CMD ["-config", "/app/config.toml"]
