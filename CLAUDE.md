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
  telegram/client.go              — NewBot: Telegram client with timeouts that redacts the token from errors
  log/log.go                      — leveled stdout/stderr loggers (Debug, Info, Warn, Error, Trace)
```

### Runtime goroutines

| Goroutine | Started in | Purpose |
|-----------|-----------|---------|
| Main loop | `main()` | Polls Frigate events every `SLEEP_TIME` seconds, queues finished ones |
| Event workers | `main()` → `StartEventWorkers()` | `EVENT_WORKERS` goroutines that send queued events |
| WatchDog loop | `main()` → `NotifyEvents()` | Polls Frigate events every `WATCH_DOG_SLEEP_TIME` seconds, sends text-only alerts |
| REST API | `main()` → `RunServer()` | Optional Gin HTTP server (only if `REST_API_ENABLE=true`) |
| Telegram bot | `main()` → `ChatBot()` | Long-polling Telegram update channel for commands |

### Data flow

1. `GetEvents(ctx, FrigateURL, bot, SetBefore)` → HTTP GET Frigate `/api/events?...&include_thumbnails=0` (response capped at 32 MB)
2. `ParseEvents(FrigateEvents, bot, WatchDog)` → filter by camera/label/zone, skip events that haven't ended `TIME_WAIT_SAVE` seconds ago, check Redis for duplicates, queue the rest
3. `SendMessageEvent(ctx, event, bot)` (in a worker) → download media (thumbnail via HTTP, or base64 if the event has one; clip with retries; preview), build `MediaGroupConfig`, send via Telegram bot with retries
4. Redis key `{EventID}` with TTL tracks seen events; `Finished` = skip

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
| `TIME_WAIT_SAVE` | `30` | Seconds after an event ends before its clip is downloaded |
| `EVENT_WORKERS` | `1` | Events sent concurrently |
| `CLIP_RETRY_TIMEOUT` | `300` | Seconds to keep retrying a failed clip download |
| `MEDIA_DOWNLOAD_TIMEOUT` | `300` | Timeout per clip/preview download |
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

2. **`ErrorSend` / `WarnSend` do not halt execution** — they log + send a Telegram message but return normally. Every caller MUST handle its own early return after an error; the functions won't stop the control flow. **Note:** Both functions are throttled — Telegram messages are rate-limited to 1 per 15 minutes per level to avoid chat flooding. Errors are always logged locally regardless of throttle state.

3. **The `GetStateSendEvent` / `SetStateSendEvent` naming is inverted**: `SetStateSendEvent(true)` means **stop** sending. `GetStateSendEvent()` returns `true` when the stop key is **not** set (i.e., sending is active). This double inversion works but is confusing — see the Redis key name `FrigateTelegramStopSendEventMessage` for the original intent.

4. **Media files go to `/tmp`** — the Docker Compose mounts a tmpfs there. Every file (thumbnail, clip, preview) is a unique `os.CreateTemp` file; never build a path from the event ID, which comes from Frigate's API. Files are cleaned up after sending.

5. **Telegram media group limit** — clips > 50 MB are skipped (Telegram limit).

6. **Event dedup** — events are tracked in Redis by their Frigate event ID. An event in `Finished` state is skipped. Events that are queued or being sent are tracked in memory (`queuedEvents`), so a slow send is never picked up twice. `InProgress` / `InWork` values are only written by older versions; `CheckEvent` still understands them.

7. **Redis circuit breaker** — after 5 consecutive Redis failures, the circuit opens and event processing pauses until Redis recovers. This prevents event spam storms when Redis is down. Check `IsRedisHealthy()` before processing.

8. **Config validation** — `Config.Validate()` runs at startup and exits with clear errors if required fields (bot token, chat ID, Frigate URL) are missing. This prevents cryptic runtime panics.

9. **Event queue** — finished events go into `eventQueue` and are sent by `EVENT_WORKERS` workers (default 1). Waiting is preferred over parallel downloads: on weak machines, parallel clip builds made Frigate too slow and clips were dropped.

10. **Graceful shutdown** — SIGINT/SIGTERM triggers context cancellation, cleanly stopping the main loop and NotifyEvents watchdog. A shutdown message is sent to Telegram. The loops' sleeps and the Frigate requests honour the context, so shutdown finishes within Docker's 10s stop timeout even with a long `SLEEP_TIME`.

11. **Clip downloads** — Frigate builds `clip.mp4` from recording segments on request and streams it while ffmpeg runs, so a download can be slow or fail until the segments are saved. `SaveClip` retries for `CLIP_RETRY_TIMEOUT` seconds, each attempt with a `MEDIA_DOWNLOAD_TIMEOUT` timeout (the 60s API timeout is too short). Media goes to unique `os.CreateTemp` files. If the clip still can't be attached, the caption says why.

## Security

- **REST API authentication**: Set `REST_API_KEY` to require `X-API-Key` header on control endpoints (/stop, /resume, /mute, /unmute, /status). /ping is always unauthenticated for healthchecks. Keys are compared as SHA-256 hashes in constant time.
- **Bot token redaction**: Telegram URLs contain the bot token and Go's HTTP errors contain the URL. Always create the bot with `telegram.NewBot`, whose HTTP client strips the token from errors before they reach logs or chat messages.
- **HTTP timeouts**: REST API has Read/Write/Idle timeouts (10s/10s/60s). Telegram requests time out 2 min after the request is sent (uploads themselves aren't limited).
- **Host networking**: Both Redis and frigate-telegram use host networking in production to match Frigate's network mode. This means ports are bound directly on the host, so Redis is started with `--bind 127.0.0.1 -::1`.
- **Docker build context**: `.dockerignore` keeps `.env` and `.git` out of the build context.

## Production Deployment

The production instance runs at `/home/manu/frigate-telegram` with:
- Docker Compose with host networking
- `.env` file with secrets (gitignored)
- Redis data persisted at `/mnt/docker/redis/data`
- REST API on port 3232 with authentication
- Log rotation: max 10MB per file, max 3 files

## Upstream

This is a fork of `github.com/OldTyT/frigate-telegram`. The upstream remote is configured.
