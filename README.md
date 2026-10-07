# EZNews

自托管的轻量新闻聚合与语音播报工具。服务端**不采集新闻**，只提供 ingest 接口接收数据；客户端主动拉取。

- **服务端**：Go，单体，单个可执行文件部署（API + Web 阅读界面 + 管理后台全部内嵌）
- **Web 端**：Vite + React + TypeScript + Tailwind（已实现）
- **Android 端**：Kotlin 原生（规划中）

## 设计要点

| 决策 | 内容 |
|---|---|
| 采集边界 | 服务端**不做**任何爬取/RSS 解析，只暴露 `POST /api/v1/ingest/articles` 接收数据，由独立采集器调用写入 |
| 推送 | **不做**。无 WebSocket、无厂商推送；客户端主动 REST 拉取 + 增量游标 |
| 存储 | SQLite 单文件（纯 Go 驱动，无 CGO），WAL 模式 |
| 部署 | `//go:embed` 内嵌全部前端产物，一个二进制跑起来 |
| 登录 | **增强而非门槛**。游客可用绝大部分功能，登录后收藏/已读/偏好跨设备同步 |
| TTS | 服务端合成音频下发，客户端只播放。`TtsProvider` 抽象，可切 mock / 阿里 / 腾讯 |

## 快速开始

### 构建

```bash
cd server
bash scripts/build.sh all          # 产出 bin/eznews（约 13 MB）
```

### 运行

```bash
# 1. 先准备配置（从模板复制，填入 API Key 与 JWT 密钥）
cp config.example.yaml config.yaml

# 2. 启动
./bin/eznews -config ./config.yaml
```

默认监听 `127.0.0.1:8080`。访问 `/` 是阅读界面，`/admin` 是管理后台。

> **纯环境变量部署**：不提供配置文件也可启动，必需项通过 `EZNEWS_*` 环境变量注入。
> 注意 JWT 密钥的环境变量名是 `EZNEWS_AUTH_JWT_SECRET`（带 `AUTH_` 前缀）。

### 冷启动第一个管理员

系统没有 SMTP、不提供邮件找回，因此没有管理员时无法进入后台。用 CLI 提升：

```bash
./bin/eznews -config ./config.yaml -promote-admin <username>
```

（**必须先跑过迁移**，即至少正常启动过一次。）

## 目录结构

```
docs/                    产品与架构文档
  PRD.md                 产品需求
  PRD-ACCOUNT.md         账号体系需求
  ARCHITECTURE.md        系统架构（19 章）
  api/openapi.yaml       API 契约（权威）
server/                  Go 服务端
  cmd/eznews/            入口
  internal/              各层实现
  migrations/            SQL 迁移
  scripts/               构建与联调脚本
  smoke/                 端到端冒烟
web/                     Web 端
```

## 开发

详见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) 与 [`docs/api/openapi.yaml`](docs/api/openapi.yaml)。

- **API 契约以 `docs/api/openapi.yaml` 为准**，实现与它冲突时以契约为准
- 冒烟脚本：`server/smoke/run.sh`、`server/smoke/admin-e2e.sh`
- 联调数据：`server/scripts/seed-dev-data.sh`（配 `server/config.dev.yaml`，独立端口与数据库）

## 许可

暂未指定。
