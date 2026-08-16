# ScopeForge 攻击工具容器镜像。
#
# 架构:平台(scopeforge)在本机跑,Agent 的 bash 命令在挑战容器内执行——
# 攻击工具只存在于本镜像,宿主机无需安装。
# 容器生命周期:一任务一容器(sandbox.EnsureContainer),challenge 结束销毁(环境重置)。
#
# 工具面:预装 Web 渗透主链(kali 包),覆盖扫描/注入/爆破/目录枚举/口令类;
# 容器内以 root + Docker 默认 capabilities 运行(禁 --privileged;cap 由
# sandbox.Create 控制——不 drop,保证 nmap -sS/-O、tcpdump 等工具全功能),
# 需要更多工具时在此追加 apt 包(保持一行一个,便于增删)后重新构建。
#
# 浏览器自动化:
#   - chromium(kali 包)+ agent-browser CLI(npm 全局,多阶段构建)
#   - agent 经容器 bash 调 `agent-browser open/snapshot/fill/click ...`
#     (skill 卡 agent-browser 引导),headless 直连 CDP
#   - 构建时冒烟:agent-browser open about:blank → get title(防镜像不可用)
#
# 构建: docker build -f deploy/executor/Dockerfile -t scopeforge-attacker .
# 启用: capability.sandbox.enabled: true(评测模式建议 network: host,
#       容器共享宿主网络直接访问靶场;默认 bridge 需配置网络连通)。
# 超时: 单条命令由容器内 `timeout -s KILL <secs>` 包装(退出码 137 = 超时)。
FROM kalilinux/kali-rolling:latest AS base

# 国内构建环境:默认 kali 源(DNS/HTTPS 证书)不可达时换清华镜像(http)。
# 已验证:mirrors.tuna.tsinghua.edu.cn/kali 索引完整。可移除:若环境能直连 http.kali.org。
RUN printf "Types: deb\nURIs: http://mirrors.tuna.tsinghua.edu.cn/kali/\nSuites: kali-rolling\nComponents: main contrib non-free non-free-firmware\nSigned-By: /usr/share/keyrings/kali-archive-keyring.gpg\n" > /etc/apt/sources.list.d/kali.sources

# Web 渗透主链(按需增删,一行一个):
#   sqlmap(SQLi) nmap(扫描) nikto(Web 扫描) dirsearch/ffuf/gobuster(目录枚举)
#   hydra(口令爆破) netcat-openbsd curl wget python3-pip git tmux jq
#   dnsutils bind-tools whois(信息收集) file procps(基础)
# 浏览器:chromium + chromium-sandbox + 字体(headless 渲染/截图)
#   nodejs npm(agent-browser 运行时)
RUN apt-get update && apt-get install -y --no-install-recommends \
    sqlmap \
    nmap \
    nikto \
    dirsearch \
    ffuf \
    gobuster \
    hydra \
    netcat-openbsd \
    curl \
    wget \
    python3 \
    python3-pip \
    git \
    tmux \
    jq \
    dnsutils \
    whois \
    file \
    procps \
    chromium \
    chromium-sandbox \
    fonts-liberation \
    fonts-noto-cjk \
    nodejs \
    npm \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# 浏览器自动化 CLI(CDP 直连 chromium,
# 专为 AI agent 设计 snapshot/refs/batch 工作流;构建期只装不冒烟,
# 冒烟在最终阶段做——需要 chromium 已就位)
FROM base AS agent-browser-builder

ARG AGENT_BROWSER_VERSION=latest

RUN npm install -g --prefix /opt/agent-browser "agent-browser@${AGENT_BROWSER_VERSION}" \
    && /opt/agent-browser/bin/agent-browser --version

# 最终镜像:工具面 + agent-browser
FROM base

COPY --from=agent-browser-builder /opt/agent-browser /opt/agent-browser
RUN ln -sf /opt/agent-browser/bin/agent-browser /usr/local/bin/agent-browser \
    && agent-browser --version \
    && agent-browser open about:blank \
    && agent-browser get title \
    && agent-browser close

# 可选工作用户(容器默认以 root 运行保证工具全功能;pf 供降权执行场景使用)
RUN useradd -m -s /bin/bash pf && mkdir -p /home/pf/workspace

WORKDIR /home/pf/workspace
ENV TZ=Asia/Shanghai
ENV AGENT_BROWSER_HEADLESS=1
ENV AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium
