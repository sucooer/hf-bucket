# Hugging Face Upload Bot

[中文版](README_CN.md) | English

A Telegram bot for uploading files to Hugging Face **Bucket** storage with Cloudflare CDN acceleration.

## Features

- Upload images, videos, audio, and documents via Telegram
- Organize files into folders (images/videos/documents/others)
- Support **multiple buckets** - user can choose which bucket to upload
- Short CDN links via Cloudflare Worker proxy
- User statistics tracking

## Quick Start

```bash
# Configure environment
cp .env.example .env
# Edit .env with your tokens

# Start with Docker
docker-compose up -d
```

## Commands

- `/start` - Start the bot
- `/help` - Help information
- `/folder` - Switch folder (images/videos/documents/others)
- `/folders` - List all folders
- `/bucket` - Switch bucket (auto-detected from your HF account)
- `/buckets` - List all available buckets
- `/status` - Current status
- `/stats` - Upload statistics

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `TELEGRAM_BOT_TOKEN` | Telegram bot token | Yes |
| `HF_TOKEN` | Hugging Face API token (auto-detects username and buckets) | Yes |
| `CDN_BASE_URL` | Cloudflare CDN URL (e.g. `https://cdn.example.com`) | No |
| `HF_FOLDERS` | Custom folders (comma-separated) | No |

**Note:** No need to configure buckets - they are auto-detected from your HF account.

## Build Docker Image

```bash
docker build -t hf-telegram-bot .
docker run -d --name hf-telegram-bot --env-file .env hf-telegram-bot
```

## File Structure

```
├── main.go                     # Bot source code (Go)
├── Dockerfile                  # Docker build file
├── docker-compose.yml          # Docker Compose config
├── cloudflare_worker.js.template # Cloudflare Worker template
└── .env                        # Environment variables
```

## CDN URL Format

Short URL: `https://cdn.example.com/{folder}/{filename}`

Maps to: `https://huggingface.co/buckets/{user}/{bucket}/resolve/{folder}/{filename}`