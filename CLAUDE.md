# CLAUDE.md — frigate-telegram

## Overview

frigate-telegram bridges [Frigate NVR](https://frigate.video/) and Telegram. It polls Frigate's `/api/events` endpoint, filters events by camera/label/zone, and forwards them as rich media messages (thumbnail, clip, preview) to a configured Telegram chat. State (stop/mute, seen events) is persisted in Redis so restarts don't re-send old events.

## Architecture

```
main.go                           — entrypoint: loads config, starts loops
internal/
  config/config.go                — env-var → Config struct (no files, no CLI flags)
  frigate/frigate.go              — HTTP client for Frigate API + event filtering/sending
  redis/redis.go                  — Redis client: stop/mute flags + event dedup TTL
  restapi/restapi.go              — Gin HTTP server: /api/v1/{ping,stop,resume,mute,unmute,status}
  telegram/telegram.go            — Telegram bot command handler (/ping, /stop, /resume, …)
  log/log.go                      — leveled stdout/stderr loggers (Debug, Info, Warn, Error, Trace)
```

### Runtime goroutines

| Goroutine | Started in | Purpose |
|-----------|-----------|---------|
| Main loop | `main()` | Polls Frigate events every `SLEEP_TIME` seconds, sends new ones |
| WatchDog loop | `main()` → `NotifyEvents()` | Polls Frigate events every `WATCH_DOG_SLEEP_TIME` seconds, sends text-only alerts |
| REST API | `main()` → `RunServer()` | Optional Gin HTTP server (only if `REST_API_ENABLE=true`) |
| Telegram bot | `main()` → `ChatBot()` | Long-polling Telegram update channel for commands |

### Data flow

1. `GetEvents(FrigateURL, bot, SetBefore)` → HTTP GET Frigate `/api/events?...`
2. `ParseEvents(FrigateEvents, bot, WatchDog)` → filter by camera/label/zone, check Redis for duplicates
3. `SendMessageEvent(event, bot)` → download media (thumbnail via base64 or HTTP, clip, preview), build `MediaGroupConfig`, send via Telegram bot
4. Redis key `{EventID}` with TTL tracks seen events; `InProgress` = allow re-send, `Finished` = skip

## Configuration

**All configuration is via environment variables.** See `internal/config/config.go:New()` for defaults.

Key variables:

| Variable | Default | Notes |
|----------|---------|-------|
| `TELEGRAM_BOT_TOKEN` | `""` | Required |
| `TELEGRAM_CHAT_ID` | `0` | Required |
| `FRIGATE_URL` | `http://localhost:5000` | Internal URL for API calls |
| `FRIGATE_EXTERNAL_URL` | `http://localhost:5000` | External URL embedded in messages |
| `REDIS_ADDR` | `localhost:6379` | |
| `SLEEP_TIME` | `5` | Seconds between main-loop polls |
| `DEBUG` | `false` | Enables debug logging + bot debug |

## Build & Run

### Local (Go 1.23+)
```bash
go build .
REDIS_ADDR=localhost:6379 TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... ./frigate-telegram
```

### Docker
```bash
docker compose up -d
```

### Dev
```bash
docker compose -f docker-compose.dev.yml up -d
```

## CI/CD

- **golang.yml** — `golangci-lint` on every push/PR
- **github-docker.yml** — multi-arch Docker build & push to `ghcr.io` on push to `main`

## Key dependencies

| Package | Purpose |
|---------|---------|
| `github.com/go-telegram-bot-api/telegram-bot-api/v5` | Telegram Bot API client |
| `github.com/redis/go-redis/v9` | Redis client |
| `github.com/gin-gonic/gin` | REST API HTTP framework |
| `github.com/swaggo/gin-swagger` | Swagger UI for REST API |

## Things to know before changing code

1. **`config.New()` creates a new struct every call** — it re-reads all env vars. This is intentional for runtime config changes but means it's called frequently throughout the codebase (even inside loops). Don't cache it globally without understanding this trade-off.

2. **`ErrorSend` / `WarnSend` do not halt execution** — they log + send a Telegram message but return normally. Every caller MUST handle its own early return after an error; the functions won't stop the control flow.

3. **The `GetStateSendEvent` / `SetStateSendEvent` naming is inverted**: `SetStateSendEvent(true)` means **stop** sending. `GetStateSendEvent()` returns `true` when the stop key is **not** set (i.e., sending is active). This double inversion works but is confusing — see the Redis key name `FrigateTelegramStopSendEventMessage` for the original intent.

4. **Media files go to `/tmp`** — the Docker Compose mounts a tmpfs there. Files are cleaned up after sending.

5. **Telegram media group limit** — clips > 50 MB are skipped (Telegram limit).

6. **Event dedup** — events are tracked in Redis by their Frigate event ID. An event in `InProgress` state will be re-sent (Frigate may update it). An event in `Finished` state is skipped. `InWork` means a goroutine is currently processing it.

## Upstream

This is a fork of `github.com/OldTyT/frigate-telegram`. The upstream remote is configured.
