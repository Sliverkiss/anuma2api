# Anuma 2API

一个将 **Anuma AI** 账号池包装为 **OpenAI 兼容 API** 的网关 + 自动注册工具。

本仓库是 [AnumaAI](https://github.com/xiaolajiaoyyds/AnumaAI) 的二次开发分支：
- 上游项目提供 Anuma AI 的批量注册与管理思路，但它的纯 HTTP 注册流程因缺少 hCaptcha token 已失效；
- 本项目改为 **camoufox 反检测浏览器 + hcaptcha-challenger** 打通注册，并用 **Go 网关** 把账号池暴露为 OpenAI 兼容的 `/v1/chat/completions`。

> 本项目仅用于个人学习与自动化研究。请遵守目标服务的服务条款，自行评估合规风险。

---

## 功能特性

- **OpenAI 兼容接口**：`GET /v1/models`、`POST /v1/chat/completions`（含 SSE 流式），可直接接入任意 OpenAI SDK。
- **账号池自动管理**：SQLite 持久化，账号支持上传、列表、删除、单查余额、手动刷新 token。
- **自动换号重试**：403 / 429 自动切换账号；401 自动刷新 token 重试；余额耗尽自动进入 cooldown。
- **月度巡检**：后台定时刷新 token 保活，cooldown 账号余额恢复后自动回到可用池。
- **模型白名单**：仅暴露白名单内的模型，tier-gate 模型（账号级 403）不会出现在 `/v1/models`。
- **网关侧自动续写**：输出达到模型 token 上限（`finish_reason=length`）时，自动追加历史重放，直到自然结束或到达段数上限。
- **自动注册脚本**：临时邮箱 → hCaptcha → Privy 认证 → 嵌入式钱包 → 查余额，一键注册并推送到网关。

---

## 架构

```
                     ┌──────────────────────────────────────────────┐
                     │            Anuma AI (上游, 闭源)              │
                     │   chat.anuma.ai   auth.privy.io              │
                     │   portal.anuma.ai/api/v1                     │
                     └───────▲──────────────────────────────────────┘
                             │ Responses API + identity_token
                             │ (privy-app-id / privy-ca-id 请求头)
        ┌────────────────────┴───────────────────────────────────┐
        │                   gateway/ (Go)                        │
        │                                                        │
        │  internal/config  环境变量配置                          │
        │  internal/upstream portal/privy 上游客户端              │
        │  internal/convert  OpenAI Chat ↔ Responses 转换 + 注入  │
        │  internal/pool     账号池 (SQLite + cooldown/巡检)       │
        │  internal/api      /v1/* OpenAI 兼容  +  /api/* 管理     │
        └───────────────▲────────────────────────────────────────┘
                        │ Bearer token (ANUMA_ADMIN_PASSWORD)
                        │ GET /v1/models / POST /v1/chat/completions
        ┌───────────────┴─────────────┐
        │  任意 OpenAI SDK / 客户端    │
        │  (curl, openai-python, ...)  │
        └─────────────────────────────┘
```

```
注册流程 (register.py):

  cf-temp-mail ──► 临时邮箱 ──► chat.anuma.ai 表单
                                      │
                          hCaptcha (隐形 / AgentV 解 challenge)
                                      │
                           auth.privy.io passwordless/init + OTP
                                      │
                              Privy 嵌入式钱包 + refresh session
                                      │
                        portal.anuma.ai credits/balance (100 credits)
                                      │
                          POST 网关 /api/accounts 推送到账号池
```

---

## 快速开始

### 1. 启动网关（Docker）

```bash
# 复制环境模板并填写真实值
cp .env.example .env
vim .env

# 构建并启动
cd gateway
docker compose up -d --build
```

网关监听 `:7895`，健康检查 `GET /healthz`。

### 2. 注册账号并推送网关

```bash
# 安装运行时依赖
pip install -r requirements.txt

# 单账号注册（成功后自动 POST 到网关 /api/accounts）
python3 register.py

# 批量注册 5 个
python3 register.py --count 5

# 从历史 CSV 批量补传网关（恢复账号池）
python3 register.py --import-csv --dry-run   # 先预览
python3 register.py --import-csv             # 实际上传
```

注册需要可用的 **camoufox** 与 **hcaptcha-challenger** 环境（路径通过环境变量注入，见下节），以及一个临时邮箱服务。

### 临时邮箱提供商

注册脚本支持两种临时邮箱提供商（通过 `MAIL_PROVIDER` 环境变量或 `--mail-provider` 参数选择）：

| 提供商 | 说明 | 必填配置 |
|--------|------|----------|
| `cftemp`（默认） | cf-temp-mail 服务 | `CF_TEMP_MAIL_API` / `CF_TEMP_MAIL_KEY` / `CF_TEMP_MAIL_DOMAIN` |
| `yydsmail` | YYDS Mail 服务（`mail_providers/yydsmail.py`） | `YYDS_MAIL_API_KEY`（`YYDS_MAIL_BASE_URL` / `YYDS_MAIL_DOMAIN` / `YYDS_MAIL_SUBDOMAIN` / `YYDS_MAIL_WILDCARD` 可选） |

```bash
# 使用 YYDS Mail
MAIL_PROVIDER=yydsmail \
YYDS_MAIL_API_KEY=your_key \
python3 register.py --count 1

# 或通过命令行参数覆盖（优先级高于环境变量）
python3 register.py --mail-provider yydsmail --count 1
```

> 注意：`yydsmail` 提供商需要 `pip install curl_cffi`（已加入 `requirements.txt`）。

### 3. 验证网关

```bash
# 健康检查
curl http://127.0.0.1:7895/healthz

# 查看账号池统计
curl -H "Authorization: Bearer YOUR_ADMIN_PASSWORD" \
     http://127.0.0.1:7895/api/stats
```

---

## 配置说明

复制 `.env.example` 为 `.env` 后填写。所有敏感值请使用自己的强随机值，**不要使用仓库内占位符**。

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `ANUMA_ADMIN_PASSWORD` | `YOUR_ADMIN_PASSWORD` | API Bearer token，部署前必须改为强随机值 |
| `ANUMA_PORT` | `7895` | HTTP 监听端口 |
| `ANUMA_DB_PATH` | `./gateway.db` | SQLite 持久化路径（容器内 `/app/data/gateway.db`） |
| `ANUMA_UPSTREAM_BASE_URL` | `https://portal.anuma.ai/api/v1` | 上游 portal base URL |
| `ANUMA_PRIVY_BASE_URL` | `https://auth.privy.io/api/v1` | Privy 认证 base URL |
| `ANUMA_MODEL_CACHE_TTL` | `600` | 模型列表缓存秒数 |
| `ANUMA_MAX_RETRIES` | `5` | 每次请求的换号/重试预算 |
| `ANUMA_AUTH_FAIL_LIMIT` | `3` | 连续鉴权失败上限，超出后账号禁用 |
| `ANUMA_MODEL_ALLOWLIST` | 12 个可用模型 | 逗号分隔的模型白名单 |
| `ANUMA_REFRESH_INTERVAL` | `1800` | 后台 token 巡检间隔秒数（`<=0` 禁用） |
| `ANUMA_COOLDOWN_CHECK_INTERVAL` | `86400` | cooldown 账号巡检间隔秒数（`<=0` 禁用） |
| `ANUMA_RETRY_BACKOFF_MS` | `300` | 重试退避基数毫秒 |
| `ANUMA_AUTO_CONTINUE` | `true` | 网关侧自动续写开关 |
| `ANUMA_AUTO_CONTINUE_MAX_SEGMENTS` | `5` | 自动续写最大段数（含首段，`<=0` 禁用） |
| `CF_TEMP_MAIL_API` | `YOUR_MAIL_DOMAIN` | 临时邮箱服务 API base |
| `CF_TEMP_MAIL_KEY` | — | 临时邮箱建箱鉴权 key（必填） |
| `CF_TEMP_MAIL_DOMAIN` | `YOUR_MAIL_DOMAIN` | 临时邮箱域名 |
| `MAIL_PROVIDER` | `cftemp` | 临时邮箱提供商：`cftemp` / `yydsmail` |
| `YYDS_MAIL_API_KEY` | — | YYDS Mail API key（`MAIL_PROVIDER=yydsmail` 时必填） |
| `YYDS_MAIL_BASE_URL` | `https://maliapi.215.im/v1` | YYDS Mail API base |
| `YYDS_MAIL_DOMAIN` | — | YYDS Mail 域名（可选） |
| `YYDS_MAIL_SUBDOMAIN` | — | YYDS Mail 子域名（可选） |
| `YYDS_MAIL_WILDCARD` | `false` | YYDS Mail 通配符建箱（可选） |
| `ANUMA_GATEWAY_URL` | `http://127.0.0.1:7895` | 注册脚本推送网关的地址 |
| `ANUMA_GATEWAY_TOKEN` | — | 推送网关用的 Bearer token（与 `ANUMA_ADMIN_PASSWORD` 一致） |
| `CAMOUFOX_SITE_PACKAGES` | — | camoufox Python site-packages 路径 |
| `HC_CHALLENGER_SRC` / `HC_CHALLENGER_VENV_SP` | — | hcaptcha-challenger src / venv site-packages 路径 |
| `HC_CHALLENGER_ENV` | — | hcaptcha-challenger 的 `.env` 路径（含 `OPENAI_BASE_URL` 等） |

---

## API 使用示例

所有 `/v1/*` 与 `/api/*` 端点都需要 Bearer 认证：

```bash
export API=http://127.0.0.1:7895
export TOKEN=YOUR_ADMIN_PASSWORD
```

### 列出模型

```bash
curl -s "$API/v1/models" \
  -H "Authorization: Bearer $TOKEN" | jq
```

### 非流式对话

```bash
curl -s "$API/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.2",
    "messages": [{"role": "user", "content": "你好，用一句话介绍你自己"}]
  }' | jq
```

### 流式对话（SSE）

```bash
curl -N "$API/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-5",
    "stream": true,
    "messages": [{"role": "user", "content": "写一段 300 字的短文"}]
  }'
```

### 管理 API

```bash
# 账号池统计
curl -s "$API/api/stats" -H "Authorization: Bearer $TOKEN"

# 上传单个账号（注册脚本即调用此接口）
curl -s -X POST "$API/api/accounts" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "anuma...@example.com",
    "wallet_address": "0x1234...",
    "identity_token": "...",
    "access_token": "...",
    "refresh_token": "...",
    "available_credits": 100,
    "tier": "basic"
  }'

# 查看 cooldown 账号
curl -s "$API/api/accounts?status=cooldown" -H "Authorization: Bearer $TOKEN"

# 手动触发一次全量 token 刷新 / cooldown 巡检
curl -s -X POST "$API/api/accounts/refresh-all"    -H "Authorization: Bearer $TOKEN"
curl -s -X POST "$API/api/accounts/cooldowns-check" -H "Authorization: Bearer $TOKEN"

# 删除账号
curl -s -X DELETE "$API/api/accounts/you@example.com" -H "Authorization: Bearer $TOKEN"
```

### 接入任意 OpenAI SDK

设置 `base_url` 指向网关即可：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:7895/v1",
    api_key="YOUR_ADMIN_PASSWORD",
)

resp = client.chat.completions.create(
    model="qwen-3.6-plus",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

---

## 仓库结构

```
anuma2api/
├── README.md                # 本文档
├── .env.example             # 环境变量模板（含所有占位符）
├── .gitignore
├── register.py              # 自动注册脚本（临时邮箱→hCaptcha→Privy→钱包→推送网关）
├── loop_register.sh         # 注册循环 wrapper（可配 systemd / cron）
├── requirements.txt
└── gateway/                 # Go 网关
    ├── cmd/server/main.go
    ├── internal/
    │   ├── api/             # HTTP 路由（/v1/* 与 /api/*）
    │   ├── config/          # 环境变量配置
    │   ├── convert/         # OpenAI Chat ↔ Responses 转换、系统提示注入、模型别名
    │   ├── pool/            # 账号池（SQLite 持久化、cooldown、巡检）
    │   └── upstream/        # portal.anuma.ai / auth.privy.io 上游客户端
    ├── Dockerfile
    ├── docker-compose.yml
    ├── go.mod / go.sum
    └── .gitignore
```

---

## 参考链接

- 上游参考仓库：[xiaolajiaoyyds/AnumaAI](https://github.com/xiaolajiaoyyds/AnumaAI)（API 网关 / 批量注册管理思路；其纯 requests 注册流程已失效）
- Anuma AI：[www.anuma.ai](https://www.anuma.ai/)（聊天入口 `chat.anuma.ai`）
- Privy 认证：[auth.privy.io](https://auth.privy.io/)
- [camoufox](https://github.com/daijro/camoufox) — 反检测浏览器（Firefox fork）
- [hcaptcha-challenger](https://github.com/QIN2DIM/hcaptcha-challenger) — hCaptcha 挑战求解器

## 知识库

项目的详细技术文档（架构设计、API参考、运维手册等）以 [OKF 格式](https://github.com/Sliverkiss/my-llm-wiki/tree/main/anuma2api) 维护在 `Sliverkiss/my-llm-wiki` 仓库的 `anuma2api/` 目录下，包含：

- [系统架构](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/architecture.md)
- [注册流程](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/registration-flow.md)
- [API 端点参考](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/api-endpoints.md)
- [环境变量参考](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/env-config.md)
- [部署指南](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/deployment.md)
- [已知问题与坑](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/known-issues.md)
- [完整目录](https://github.com/Sliverkiss/my-llm-wiki/blob/main/anuma2api/index.md)

---

## 免责声明

本项目仅供学习与研究。使用本项目可能违反目标服务条款，由此产生的一切风险与责任由使用者自行承担。
