#!/usr/bin/env bash
# deploy.sh - 服务器一键拉取更新并重启
# 用法: bash deploy.sh
set -e

SERVICE="eth-perp-system"
BINARY="./eth-perp-system"
GO_BUILD_CMD="go build -o eth-perp-system ./cmd"

echo "==> [1/4] 暂存本地修改（如有）..."
git stash --include-untracked 2>/dev/null || true

echo "==> [2/4] 拉取最新代码..."
git pull origin main

echo "==> [3/4] 重新编译..."
# 优先用系统 Go；若不在 PATH 则尝试常见安装位置
if ! command -v go &>/dev/null; then
    export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin"
fi
$GO_BUILD_CMD
echo "    编译完成: $BINARY"

echo "==> [4/4] 重启服务..."
# 如果注册了 systemd 服务，用 systemctl 重启；否则直接 kill+后台启动
if systemctl is-active --quiet "$SERVICE" 2>/dev/null || systemctl is-enabled --quiet "$SERVICE" 2>/dev/null; then
    systemctl restart "$SERVICE"
    echo "    systemctl restart $SERVICE 完成"
else
    # 杀掉旧进程
    pkill -f "eth-perp-system" 2>/dev/null || true
    sleep 1
    # 后台启动（日志写到 nohup.out）
    nohup $BINARY > nohup.out 2>&1 &
    echo "    直接启动 PID=$! 日志: nohup.out"
fi

echo ""
echo "✓ 部署完成"
