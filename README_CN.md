# Hugging Face 上传 Bot

通过 Telegram 上传文件到 Hugging Face 数据集，支持 Cloudflare CDN 加速。

## 功能

- 上传图片、视频、音频、文档
- 按文件夹分类（images/videos/documents/others）
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
- `/folder` - 切换文件夹
- `/folders` - 查看所有文件夹
- `/status` - 当前状态
- `/stats` - 上传统计

## 环境变量

| 变量 | 说明 | 必需 |
|------|------|------|
| `TELEGRAM_BOT_TOKEN` | Telegram Bot Token | 是 |
| `HF_TOKEN` | Hugging Face API Token | 是 |
| `HF_REPO_ID` | Hugging Face 仓库 (如 `user/dataset`) | 是 |
| `CDN_BASE_URL` | Cloudflare CDN 域名 | 否 |
| `HF_FOLDERS` | 自定义文件夹（逗号分隔） | 否 |

## 构建 Docker 镜像

```bash
docker build -t hf-bot .
docker run -d --name hf-bot --env-file .env hf-bot
```

## 文件结构

```
├── main.go              # Bot 源代码 (Go)
├── Dockerfile           # Docker 构建文件
├── docker-compose.yml   # Docker Compose 配置
├── cloudflare_worker.js # Cloudflare Worker CDN 代理
└── .env                 # 环境变量
```