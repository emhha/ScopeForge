# Netloom 接入说明

## 依赖与获取

模块：`github.com/emhha/netloom`，固定版本 `v0.0.0-20261119144832-0e71bf4ea53d`。
源码实际从 `https://192.168.1.100:10002/emhha/netloom` 拉取。
`go.mod` / `go.sum` 保留正式模块路径和校验值；不使用本地 replace、vendor 或兄弟项目源码副本。

Make 的 Go 命令通过 `scripts/go-netloom.sh` 临时设置 Git URL 映射与私有模块范围，保留调用方已有 Git 环境配置，不修改全局 Git / Go 配置。单独运行 Go 命令时可用：

```sh
./scripts/go-netloom.sh go mod download github.com/emhha/netloom
./scripts/go-netloom.sh go test ./...
```

内网镜像使用私有 CA。构建机应信任该 CA，或通过 `GIT_SSL_CAINFO=/path/to/ca.pem` 指定可信 CA；脚本不关闭证书校验。CI runner 同样需要能访问该镜像及信任其 CA。`NETLOOM_REPOSITORY` 可覆盖取源码的镜像地址，模块名和版本不变。

## 功能边界

- `http_request` 直接使用 netloom 的 `nethttp.BuildRequest`、Engine、原生重定向和正文解码；不调用系统 curl，不自行实现 HTTP 协议、认证、TLS、重试或代理传输。
- 全局录制代理使用 `netloom/mitm`；证书生成、签发、CONNECT、TLS 与请求转发协议均由库提供。项目只负责生命周期、出口代理选择和将响应流写入原有 SQLite/blob 流量存储。
- `search_mapping` 调用 `netloom/mapping` 的 FOFA、Hunter、Quake 客户端；查询转换、认证、分页和响应解析由库实现。项目负责配置、并发与关闭、结果合并和工具输出。
- 不接入企业查询、额外扫描引擎、被动分析平台、知识库或查询结果前端。

## HTTP 工具

支持方法、URL、重复头、query、form、multipart、utf8/base64 正文、Basic 认证、HTTP 版本、显式跳过证书验证、连接复用控制与有限重定向。每跳应用已有全局/任务资产规则。工具支持现有审批、按 Agent 的工具绑定和调用计量。

默认超时 30 秒，最多 120 秒；不自动重放请求。输出正文预览最多 16 KiB，原始响应采集最多 8 MiB；超过采集限制时明确返回 `complete=false`。压缩正文的展示由库进行有界解码，录制证据保留原始字节。

流量捕获开启且录制存储可用时，每跳直接写入流量库并返回 `exchange_ids`，不经过本机录制代理造成双重录制。捕获关闭时工具仍可发送请求，`exchange_ids` 为空。独立出口代理沿用系统的全局代理设置。

## MITM 与已有数据

新公共 CA 文件为 `<data>/traffic/_ca/netloom-ca-cert.pem`；私钥和持久化 CA 保存在同目录下权限为 0600 的 `netloom-ca.pem`，公共证书文件不含私钥。客户端需信任新的 netloom CA；Agent 的代理环境和 CA 路径由现有运行时自动注入。

原有流量索引、blob、搜索、删除与归档继续使用；新增 `capture_complete` 字段区分完整采集、部分采集和旧记录的未知状态。部分流量不会被当成完整证据。代理转发响应采用流式录制，不把完整大响应堆在内存中。

录制代理沿用此前扫描流量的目标 TLS 行为；`http_request` 默认验证目标证书，须显式设置 `insecure_tls=true` 才跳过。netloom 原生直连地址策略保持生效，不增加本地绕过或静默透传实现。

## 测绘配置与调用

系统设置仅提供 FOFA / Hunter / Quake 的 Key、FOFA Email、启用、API 地址、出口代理及超时配置。不提供测绘查询或结果页面，查询只通过 Agent 工具发起。

- `list_recon_sources`：只返回数据源 ID、启用状态和 Key 是否已配置。
- `search_mapping`：`query` 为域名或 IP；`provider` 为 `all`、`fofa`、`hunter` 或 `quake`；`limit` 为 1–200，默认 20；复杂查询须指定单源并设置 `native=true`。
- 测绘结果是候选资产，可通过现有 `insert_assets` 工具登记；不自动创建任务或登记资产。
- Key 不出现在配置读取结果或工具输出中。留空保持现有 Key，清除 Key 时同时停用数据源。配置持久化沿用项目设置存储。
- 可用 `ARTEX_FOFA_KEY`、`ARTEX_FOFA_EMAIL`、`ARTEX_HUNTER_KEY`、`ARTEX_QUAKE_KEY` 提供环境凭据。数据库保存的非空值优先；环境凭据不会因保存其他配置而写入数据库。未显式保存配置时，存在环境 Key 的源自动启用。

三个工具默认绑定 `auto`、`pentest`、`worker`、`mainagent`、`planner`，可在工具管理中调整绑定或停用。
