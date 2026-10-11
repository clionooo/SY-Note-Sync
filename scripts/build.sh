#!/usr/bin/env bash
#
# SY Note Sync 一键构建：服务端镜像 + 插件包
#
# 用法：
#   bash scripts/build.sh             # 构建全部
#   bash scripts/build.sh server      # 只构建服务端
#   bash scripts/build.sh plugin      # 只构建插件
#
# 说明：
#  - 需要 docker（服务端编译用 golang 镜像，插件构建用 node 镜像）
#  - 插件在 /tmp 下构建：飞牛/群晖这类带 ACL 的卷会把 node_modules 里
#    esbuild 的可执行位吃掉，导致 npm install 报 EACCES
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WHAT="${1:-all}"
GO_IMG="${GO_IMG:-golang:1.22-alpine}"
NODE_IMG="${NODE_IMG:-node:20-alpine}"
NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmmirror.com}"

build_server() {
  echo "==> 构建服务端镜像"
  ( cd "$ROOT" && docker compose build )
  echo "==> 服务端镜像完成"
}

build_plugin() {
  echo "==> 构建插件（在 /tmp 里构建，避开 ACL 卷的可执行位问题）"
  local work=/tmp/sns-plugin-build
  rm -rf "$work"
  mkdir -p "$work"
  cp -r "$ROOT/plugin" "$work/plugin"
  cp -r "$ROOT/scripts" "$work/scripts"
  rm -rf "$work/plugin/node_modules" "$work/plugin/dist"

  docker run --rm -v "$work":/proj -w /proj/plugin "$NODE_IMG" sh -c \
    "npm config set registry $NPM_REGISTRY && npm install --no-audit --no-fund >/dev/null && npm run build"

  docker run --rm -v "$work":/proj -w /proj "$NODE_IMG" sh -c \
    "apk add --no-cache zip >/dev/null 2>&1 && node scripts/package-plugin.mjs"

  rm -rf "$ROOT/plugin/dist"
  mkdir -p "$ROOT/plugin/dist" "$ROOT/dist"
  cp -r "$work/plugin/dist/." "$ROOT/plugin/dist/"
  cp "$work"/dist/*.zip "$ROOT/dist/"
  echo "==> 插件产物："
  ls -1 "$ROOT/dist"
}

case "$WHAT" in
  server) build_server ;;
  plugin) build_plugin ;;
  all)    build_server; build_plugin ;;
  *) echo "用法: bash scripts/build.sh [all|server|plugin]"; exit 1 ;;
esac
