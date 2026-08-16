#!/usr/bin/env bash
# 部署初始化(docs/06 §5.3 配置交付):生成 data/scopeforge.yaml 与 data/.env。
# serve --config /data 会从 /data/.env 读取密钥(与 reasonix 的 home .env 同风格)。
set -eu
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p data
if [ ! -f data/scopeforge.yaml ]; then
  cp "$ROOT/configs/scopeforge.yaml.example" data/scopeforge.yaml
fi
if [ ! -f data/.env ]; then
  cp "$ROOT/.env.example" data/.env
  chmod 600 data/.env
fi
echo "初始化完成:"
echo "  1) 编辑 data/scopeforge.yaml 的 providers(至少一个实例)"
echo "  2) 编辑 data/.env 填入 DEEPSEEK_API_KEY 与 SCOPEFORGE_TOKEN"
echo "  3) docker compose up -d --build"
