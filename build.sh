#!/usr/bin/env bash
# ScopeForge 一键构建:前端(vite → embed webdist)+ Go 单二进制 + 攻击容器镜像。
#
# 用法:
#   ./build.sh                # 默认:web + go
#   ./build.sh web            # 只构建前端(embed 静态资源)
#   ./build.sh go             # 只构建 Go 二进制(bin/scopeforge)
#   ./build.sh docker         # 只构建攻击容器镜像(scopeforge-attacker)
#   ./build.sh all            # web + go + docker(耗时最长)
#
# 说明:
#   - 前端产物输出到 internal/api/webdist(go:embed,单二进制无静态依赖)
#   - docker 构建在本机 buildx 数据目录只读时自动回退 legacy builder
#     (DOCKER_BUILDKIT=0)
#   - playwright driver(浏览器工具用)不在此构建;首次运行浏览器工具前执行:
#       go run ./cmd/scopeforge 或参考 docs/implementation-log 06.8
set -euo pipefail
cd "$(dirname "$0")"

MODE="${1:-default}"
GO_CMD="${GO_CMD:-go}"
NPM_CMD="${NPM_CMD:-npm}"
BIN_OUT="${BIN_OUT:-deploy/scopeforge}"

log()  { printf '\033[36m[build]\033[0m %s\n' "$*"; }
die()  { printf '\033[31m[build] 失败: %s\033[0m\n' "$*" >&2; exit 1; }
ok()   { printf '\033[32m[build] ✓ %s\033[0m\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "缺少依赖: $1"
}

build_web() {
  need "$NPM_CMD"
  log "构建前端(vite → internal/api/webdist)…"
  (cd web && npm run build)
  ok "前端构建完成(webdist 已更新,embed 单二进制随 go build 生效)"
}

build_go() {
  need "$GO_CMD"
  log "构建 Go 二进制 → $BIN_OUT"
  mkdir -p bin
  go build -o "$BIN_OUT" ./cmd/scopeforge || die "go build 失败(先检查: go vet ./...)"
  go vet ./... >/dev/null 2>&1 || log "提示: go vet 有告警(不阻断构建)"
  ok "Go 二进制构建完成: $BIN_OUT"
}

build_docker() {
  need docker
  docker version >/dev/null 2>&1 || die "docker 不可用"
  log "构建攻击容器镜像 scopeforge-attacker(deploy/executor/Dockerfile)…"
  # 本机 /home/ubuntu/.docker 只读时 buildx 无法写 activity 记录 → 回退 legacy
  if ! docker build -f deploy/executor/Dockerfile -t scopeforge-attacker . 2>/tmp/pf-buildx.err; then
    if grep -q "read-only file system" /tmp/pf-buildx.err 2>/dev/null; then
      log "buildx 数据目录只读,回退 legacy builder(DOCKER_BUILDKIT=0)…"
      DOCKER_BUILDKIT=0 docker build -f deploy/executor/Dockerfile -t scopeforge-attacker .
    else
      cat /tmp/pf-buildx.err >&2
      die "docker build 失败(见上方输出)"
    fi
  fi
  rm -f /tmp/pf-buildx.err
  ok "攻击容器镜像构建完成"
}

case "$MODE" in
  web)     build_web ;;
  go)      build_go ;;
  docker)  build_docker ;;
  all)     build_web; build_go; build_docker ;;
  default) build_web; build_go ;;
  *) die "未知参数: $MODE(支持 web|go|docker|all,缺省 web+go)" ;;
esac

log "全部完成。二进制: $BIN_OUT"
