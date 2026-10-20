#!/bin/sh
# 在仓库根目录执行，产出 build/app（linux/amd64 静态二进制）
set -e
cd "$(dirname "$0")/.."
mkdir -p build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/app ./cmd/server
ls -la build/app
