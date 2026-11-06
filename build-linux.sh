#!/bin/sh
# 编译 Linux 服务端（可在任意平台交叉编译，也可在 Linux 上直接运行本脚本）
# 同时产出 amd64 和 arm64，配合 install-linux.sh 使用
cd "$(dirname "$0")"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o peacefulVA-server-linux-amd64 . || exit 1
echo "完成: peacefulVA-server-linux-amd64"
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o peacefulVA-server-linux-arm64 . || exit 1
echo "完成: peacefulVA-server-linux-arm64"
