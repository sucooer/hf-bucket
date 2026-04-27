# Hugging Face 上传 Bot

通过 Telegram 上传文件到 Hugging Face Buckets，并可选配 Cloudflare CDN 加速。

## 功能

- 上传图片、视频、音频、文档
- 支持顶层目录和子目录分类
- 通过 Cloudflare Worker 生成短链接
- 支持 Telegram 用户/聊天白名单控制
- 通过有限 worker 队列限制并发上传
- 持久化保存用户目录、首选 bucket、统计信息和轮询 offset
- 容器重启后可继续运行

## 快速开始

```bash
cp .env.example .env
mkdir -p data
# 编辑 .env，填入 token 和 bucket 配置

docker compose up -d --build
```

## 使用 Go 直接运行

前置条件：
- 已安装 Go，且 `go` 在 `PATH` 中
- 已安装 `python3`
- 已安装 `huggingface_hub`，并且 `hf` 命令在 `PATH` 中

示例：

```bash
cp .env.example .env
mkdir -p data
# 编辑 .env，填入 token 和 bucket 配置

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
- `/mkdir` - 显式创建或切换多级目录
- `/bucket` - 切换存储桶
- `/buckets` - 查看所有存储桶
- `/status` - 当前状态
- `/stats` - 上传统计

目录输入示例：
- `2024/01/`：在当前目录下继续追加子目录
- `/images/2024/01/`：直接切换到绝对多级目录
- `/mkdir 2024/01/`：显式追加多级子目录
- `/mkdir /images/2024/01/`：显式切换到绝对多级目录

## 环境变量

| 变量 | 说明 | 必需 |
|------|------|------|
| `TELEGRAM_BOT_TOKEN` | Telegram Bot Token | 是 |
| `HF_TOKEN` | Hugging Face API Token | 是 |
| `HF_USERNAME` | Hugging Face 用户名。建议填写，避免启动时再查询 | 否 |
| `HF_BUCKETS` | 允许使用的 bucket 列表，逗号分隔。不填则启动时自动拉取 | 否 |
| `HF_DEFAULT_BUCKET` | 无法自动发现 bucket 时使用的默认值 | 否 |
| `CDN_BASE_URL` | Cloudflare Worker 域名，不要带结尾斜杠 | 否 |
| `HF_FOLDERS` | 允许使用的顶层文件夹，逗号分隔 | 否 |
| `TELEGRAM_ALLOWED_USER_IDS` | 允许访问的 Telegram 用户 ID，逗号分隔。不填则允许所有用户 | 否 |
| `TELEGRAM_ALLOWED_CHAT_IDS` | 允许访问的 Telegram 聊天 ID，逗号分隔。不填则允许所有聊天 | 否 |
| `MAX_CONCURRENT_UPLOADS` | 同时处理的最大上传并发数 | 否 |
| `STATE_FLUSH_INTERVAL_SECONDS` | 后台状态落盘间隔，单位秒 | 否 |
| `STATE_FILE` | 持久化状态文件路径 | 否 |

## Cloudflare Worker

Bot 现在生成的 CDN 路径格式为 `/<bucket>/<folder>/<file>`。

1. 在 Worker 环境变量中设置 `HF_USER`；只有需要兼容旧的 `/folder/file` 链接时才设置 `HF_DEFAULT_BUCKET`。也可以直接编辑 `cloudflare_worker.js` 里的常量。
2. 将 Worker 部署到你的 CDN 域名下。
3. 在 `.env` 中把 `CDN_BASE_URL` 设为该域名，例如 `https://cdn.example.com`。

说明：
- `HF_USER` 是必填。
- `HF_DEFAULT_BUCKET` 是可选项，只有你想兼容旧的 `/folder/file` 链接时才需要。Bot 现在默认生成 `/<bucket>/<folder>/<file>` 形式的链接。
- 中文和特殊符号文件名会保留原样存储，并在生成链接时自动做百分号编码。

## Docker

`docker-compose.yml` 会把 `./data` 挂载到 `/app/data`，用于保存 Bot 状态。
镜像内部不再内置任何运行时环境变量默认值，启动时必须通过 `.env`、`--env-file` 或显式 `-e` 参数提供。

```bash
docker build -t hf-bot .
docker run -d --name hf-bot --env-file .env -v "$(pwd)/data:/app/data" hf-bot
```

## 文件结构

```text
├── main.go                     # Bot 源代码 (Go)
├── Dockerfile                  # Docker 构建文件
├── docker-compose.yml          # Docker Compose 配置
├── cloudflare_worker.js        # Cloudflare Worker CDN 代理
├── cloudflare_worker.js.template
└── data/state.json             # 运行时持久化状态
```
