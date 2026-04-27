# Hugging Face Upload Bot

A Telegram bot for uploading files to a Hugging Face dataset with optional Cloudflare CDN acceleration.

## Features

- Upload images, videos, audio, and documents via Telegram
- Organize uploads into folders
- Generate short CDN links through a Cloudflare Worker proxy
- Restrict access with Telegram user/chat allowlists
- Limit concurrent uploads with a bounded worker queue
- Persist user folder selection, stats, and polling offset

## Quick Start

```bash
cp .env.example .env
mkdir -p data
# Edit .env with your tokens and dataset settings

docker compose up -d --build
```

## Run With Go

Prerequisites:
- Go installed and available on `PATH`

Example:

```bash
cp .env.example .env
mkdir -p data
# Edit .env with your tokens and dataset settings

export PATH=/usr/local/go/bin:$PATH
set -a
source .env
set +a
go run .
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
| `HF_REPO_ID` | Hugging Face dataset repo, e.g. `user/dataset` | Yes |
| `CDN_BASE_URL` | Cloudflare Worker base URL, without trailing slash | No |
| `HF_FOLDERS` | Allowed top-level folders, comma-separated | No |
| `TELEGRAM_ALLOWED_USER_IDS` | Allowed Telegram user IDs, comma-separated. If empty, all users are allowed | No |
| `TELEGRAM_ALLOWED_CHAT_IDS` | Allowed Telegram chat IDs, comma-separated. If empty, all chats are allowed | No |
| `MAX_CONCURRENT_UPLOADS` | Number of parallel uploads processed at once | No |
| `STATE_FLUSH_INTERVAL_SECONDS` | Background state flush interval in seconds | No |
| `STATE_FILE` | JSON state file path for persisted bot state | No |

## Cloudflare Worker

The bot generates CDN paths in the form `/<folder>/<file>` for a single dataset repository.

1. Set `HF_REPO_ID` in the Worker environment, or edit the constant in `cloudflare_worker.js`.
2. Optionally set `HF_TYPE` and `HF_BRANCH` if you need non-default values.
3. Deploy the Worker behind your CDN domain.
4. Set `CDN_BASE_URL` in `.env` to that domain.

Notes:
- `HF_REPO_ID` is required.
- Chinese characters and special symbols in filenames are preserved in storage and percent-encoded in generated URLs.
- The Worker only forwards safe headers (`Accept` and `Range`) to Hugging Face.

## Docker

The compose file mounts `./data` into `/app/data` so bot state survives restarts.
The image does not embed any default runtime environment variables. Provide them via `.env`, `--env-file`, or explicit `-e` flags.

```bash
docker build -t hf-bot .
docker run -d --name hf-bot --env-file .env -v "$(pwd)/data:/app/data" hf-bot
```
