# Hugging Face 上传 Bot

[English](README.md) | 中文版

通过 Telegram 上传文件到 Hugging Face **Bucket** 存储，支持 Cloudflare CDN 加速。

## 功能

- 上传图片、视频、音频、文档
- 按文件夹分类（images/videos/documents/others）
- 支持**多个存储桶** - 用户可选择上传到哪个 bucket
- Cloudflare Worker 代理生成短链接
- 用户上传统计

## 快速开始

```bash
# 配置环境变量
cp .env.example .env
# 编辑 .env 填入你的 token

# Docker 启动
docker-compose up -d
```

## 命令

- `/start` - 启动 Bot
- `/help` - 帮助信息
- `/folder` - 切换文件夹（images/videos/documents/others）
- `/folders` - 查看所有文件夹
- `/bucket` - 切换存储桶（自动从 HF 账户获取）
- `/buckets` - 查看所有存储桶
- `/status` - 当前状态
- `/stats` - 上传统计

## 环境变量

| 变量 | 说明 | 必需 |
|------|------|------|
| `TELEGRAM_BOT_TOKEN` | Telegram Bot Token | 是 |
| `HF_TOKEN` | Hugging Face API Token（自动获取用户名和存储桶） | 是 |
| `CDN_BASE_URL` | Cloudflare CDN 域名（如 `https://cdn.example.com`） | 否 |
| `HF_FOLDERS` | 自定义文件夹（逗号分隔） | 否 |

**注意：** 无需配置存储桶列表，会自动从 HF 账户获取。

## 构建 Docker 镜像

```bash
docker build -t hf-telegram-bot .
docker run -d --name hf-telegram-bot --env-file .env hf-telegram-bot
```

## 文件结构

```
├── main.go                     # Bot 源代码 (Go)
├── Dockerfile                  # Docker 构建文件
├── docker-compose.yml          # Docker Compose 配置
├── cloudflare_worker.js.template # Cloudflare Worker 模板
└── .env                        # 环境变量
```

## CDN 链接格式

短链接: `https://cdn.example.com/{folder}/{filename}`

对应: `https://huggingface.co/buckets/{user}/{bucket}/resolve/{folder}/{filename}`