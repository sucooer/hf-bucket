# Hugging Face 上传 Bot

通过 Telegram 上传文件到单个 Hugging Face dataset，并可选配 Cloudflare CDN 加速。

## 功能

- 上传图片、视频、音频、文档
- 按文件夹组织上传内容
- 通过 Cloudflare Worker 生成短链接
- 支持 Telegram 用户/聊天白名单控制
- 通过有限 worker 队列限制并发上传
- 持久化保存用户目录、统计信息和轮询 offset

## 快速开始

```bash
cp .env.example .env
mkdir -p data
# 编辑 .env，填入 token 和 dataset 配置

docker compose up -d --build
```

## 使用 Go 直接运行

前置条件：
- 已安装 Go，且 `go` 在 `PATH` 中

示例：

```bash
cp .env.example .env
mkdir -p data
# 编辑 .env，填入 token 和 dataset 配置

export PATH=/usr/local/go/bin:$PATH
set -a
source .env
set +a
go run .
```

## 命令

- `/start` - 启动 Bot
- `/help` - 帮助信息
- `/folder` - 切换文件夹
- `/folders` - 查看所有文件夹
- `/status` - 当前状态
- `/stats` - 上传统计

## 环境变量

| 变量 | 说明 | 必需 |
|------|------|------|
| `TELEGRAM_BOT_TOKEN` | Telegram Bot Token | 是 |
| `HF_TOKEN` | Hugging Face API Token | 是 |
| `HF_REPO_ID` | Hugging Face dataset 仓库，例如 `user/dataset` | 是 |
| `CDN_BASE_URL` | Cloudflare Worker 域名，不要带结尾斜杠 | 否 |
| `HF_FOLDERS` | 允许使用的顶层文件夹，逗号分隔 | 否 |
| `TELEGRAM_ALLOWED_USER_IDS` | 允许访问的 Telegram 用户 ID，逗号分隔。不填则允许所有用户 | 否 |
| `TELEGRAM_ALLOWED_CHAT_IDS` | 允许访问的 Telegram 聊天 ID，逗号分隔。不填则允许所有聊天 | 否 |
| `MAX_CONCURRENT_UPLOADS` | 同时处理的最大上传并发数 | 否 |
| `STATE_FLUSH_INTERVAL_SECONDS` | 后台状态落盘间隔，单位秒 | 否 |
| `STATE_FILE` | JSON 状态文件路径 | 否 |

## Cloudflare Worker

Bot 生成的 CDN 路径格式为 `/<folder>/<file>`，对应单个 dataset 仓库。

1. 在 Worker 环境变量中设置 `HF_REPO_ID`，或直接编辑 `cloudflare_worker.js` 中的常量。
2. 如有需要，可额外设置 `HF_TYPE` 和 `HF_BRANCH`。
3. 将 Worker 部署到你的 CDN 域名下。
4. 在 `.env` 中把 `CDN_BASE_URL` 设为该域名。

说明：
- `HF_REPO_ID` 是必填。
- 中文和特殊符号文件名会保留原样存储，并在生成链接时自动做百分号编码。
- Worker 只会向 Hugging Face 转发安全请求头（`Accept` 和 `Range`）。

## Docker

`docker-compose.yml` 会把 `./data` 挂载到 `/app/data`，用于保存 Bot 状态。
镜像内部不再内置任何运行时环境变量默认值，启动时必须通过 `.env`、`--env-file` 或显式 `-e` 参数提供。

```bash
docker build -t hf-bot .
docker run -d --name hf-bot --env-file .env -v "$(pwd)/data:/app/data" hf-bot
```
