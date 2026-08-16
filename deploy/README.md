# ScopeForge 部署手册(docs/06 §5.2/§5.4)

## 形态 A(赛事):docker compose
```bash
cd deploy
./init.sh              # 生成 data/scopeforge.yaml 与 data/.env
# 编辑 data/.env 填入密钥
docker compose up -d --build
# 打开 http://localhost:8080(前端 embed,无静态文件依赖)
```

## 形态 B(单机):单二进制 + SQLite
```bash
# 构建(前端产物经 go:embed 打进二进制)
cd web && npm ci && npm run build && cd ..
go build -o scopeforge ./cmd/scopeforge

# 初始化数据目录:配置 + .env
# (不指定 --config 时,serve/run 首次启动会自动创建 ~/.scopeforge/scopeforge.yaml 与 ~/.scopeforge/.env)
mkdir -p pf-data
cp configs/scopeforge.yaml.example pf-data/scopeforge.yaml
cp .env.example pf-data/.env
chmod 600 pf-data/.env
# 编辑 pf-data/scopeforge.yaml 的 providers,并填写 pf-data/.env

# 运行(--config 是配置文件夹,不是文件)
./scopeforge serve --addr :8080 --config pf-data
./scopeforge run  --config pf-data --task "对 http://target 做授权检测"
./scopeforge doctor --config pf-data
```

## 形态 C(生产授权渗透,实验性):多 Worker 共享 SQLite
多节点共享 SQLite 文件需网络文件锁(实验性);M4 演进路线提供 PostgreSQL 适配层。

## 全新机器 30 分钟起服务核对清单(§5.4)
1. [ ] Go 1.26+ / Node 24+ / Docker 安装
2. [ ] `deploy/init.sh` 生成配置
3. [ ] `~/.scopeforge/.env`(或 `--config DIR` 的 `DIR/.env`)填入 DEEPSEEK_API_KEY 与 SCOPEFORGE_TOKEN(永不入仓)
4. [ ] `./scopeforge doctor` 全绿(mock 环境缺二进制项可接受,功能降级)
5. [ ] `deploy/init.sh && docker compose up -d --build` 或单二进制 serve 可访问
6. [ ] 仪表盘:任务列表 / 审批中心 / 系统页可打开
```
