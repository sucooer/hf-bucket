# Hugging Face Upload Bot

A Telegram bot for uploading files to Hugging Face datasets with Cloudflare CDN acceleration.

## Features

- Upload images, videos, audio, and documents via Telegram
- Organize files into folders (images/videos/documents/others)
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
- `/folder` - Switch folder
- `/folders` - List all folders
- `/status` - Current status
- `/stats` - Upload statistics

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `TELEGRAM_BOT_TOKEN` | Telegram bot token | Yes |
| `HF_TOKEN` | Hugging Face API token | Yes |
| `HF_REPO_ID` | Hugging Face repo (e.g. `user/dataset`) | Yes |
| `CDN_BASE_URL` | Cloudflare CDN URL | No |
| `HF_FOLDERS` | Custom folders (comma-separated) | No |

## Build Docker Image

```bash
docker build -t hf-bot .
docker run -d --name hf-bot --env-file .env hf-bot
```

## File Structure

```
├── main.go              # Bot source code (Go)
├── Dockerfile           # Docker build file
├── docker-compose.yml   # Docker Compose config
├── cloudflare_worker.js # Cloudflare Worker for CDN
└── .env                 # Environment variables
```