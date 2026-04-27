# Hugging Face Upload Bot

A Telegram bot for uploading files to Hugging Face Buckets with optional Cloudflare CDN acceleration.

## Features

- Upload images, videos, audio, and documents via Telegram
- Organize uploads into folders and nested subfolders
- Generate short CDN links through a Cloudflare Worker proxy
- Restrict uploads with Telegram user/chat allowlists
- Limit concurrent uploads with a bounded worker queue
- Persist user folder selection, preferred bucket, stats, and polling offset
- Resume cleanly after container restarts

## Quick Start

```bash
cp .env.example .env
mkdir -p data
# Edit .env with your tokens and bucket settings

docker compose up -d --build
```

## Run With Go

Prerequisites:
- Go installed and available on `PATH`
- `python3` installed
- `hf` CLI available on `PATH` via `huggingface_hub`

Example:

```bash
cp .env.example .env
mkdir -p data
# Edit .env with your tokens and bucket settings

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
- `/mkdir` - Create or switch nested folders explicitly
- `/bucket` - Switch bucket
- `/buckets` - List all buckets
- `/status` - Current status
- `/stats` - Upload statistics

Directory input examples:
- `2024/01/` appends subfolders under the current folder
- `/images/2024/01/` switches directly to an absolute nested path
- `/mkdir 2024/01/` explicitly appends nested folders
- `/mkdir /images/2024/01/` explicitly switches to an absolute nested path

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `TELEGRAM_BOT_TOKEN` | Telegram bot token | Yes |
| `HF_TOKEN` | Hugging Face API token | Yes |
| `HF_USERNAME` | Hugging Face username. Recommended to avoid startup lookup | No |
| `HF_BUCKETS` | Allowed bucket list, comma-separated. If omitted, the bot fetches buckets from Hugging Face | No |
| `HF_DEFAULT_BUCKET` | Fallback bucket when discovery is unavailable | No |
| `CDN_BASE_URL` | Cloudflare Worker base URL, without trailing slash | No |
| `HF_FOLDERS` | Allowed top-level folders, comma-separated | No |
| `TELEGRAM_ALLOWED_USER_IDS` | Allowed Telegram user IDs, comma-separated. If empty, all users are allowed | No |
| `TELEGRAM_ALLOWED_CHAT_IDS` | Allowed Telegram chat IDs, comma-separated. If empty, all chats are allowed | No |
| `MAX_CONCURRENT_UPLOADS` | Number of parallel uploads processed at once | No |
| `STATE_FLUSH_INTERVAL_SECONDS` | Background state flush interval in seconds | No |
| `STATE_FILE` | JSON state file path for persisted bot state | No |

## Cloudflare Worker

The bot now generates CDN paths in the form `/<bucket>/<folder>/<file>`.

1. Set `HF_USER` in your Worker environment, and only set `HF_DEFAULT_BUCKET` if you want to support legacy `/folder/file` links. You can also edit the constants in `cloudflare_worker.js`.
2. Deploy the Worker behind your CDN domain.
3. Set `CDN_BASE_URL` in `.env` to that domain, for example `https://cdn.example.com`.

Notes:
- `HF_USER` is required.
- `HF_DEFAULT_BUCKET` is optional and only needed if you want legacy `/folder/file` links. The bot now emits `/<bucket>/<folder>/<file>` links by default.
- Chinese characters and special symbols in filenames are preserved in storage and percent-encoded in generated URLs.

## Docker

The compose file mounts `./data` into `/app/data` so bot state survives restarts.
The image does not embed any default runtime environment variables. Provide them via `.env`, `--env-file`, or explicit `-e` flags.

```bash
docker build -t hf-bot .
docker run -d --name hf-bot --env-file .env -v "$(pwd)/data:/app/data" hf-bot
```

## File Structure

```text
├── main.go                     # Bot source code (Go)
├── Dockerfile                  # Docker build file
├── docker-compose.yml          # Docker Compose config
├── cloudflare_worker.js        # Cloudflare Worker for CDN
├── cloudflare_worker.js.template
└── data/state.json             # Persisted runtime state
```
