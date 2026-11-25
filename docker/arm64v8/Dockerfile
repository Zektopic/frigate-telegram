FROM arm64v8/golang:1.27.4-alpine3.24 AS builder

WORKDIR /src

# Download modules in their own layer, so code changes don't download them
# again (slow on emulated ARM builds).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary; -s -w strips symbols and debug info like the old strip step.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /frigate-telegram .

# Alpine instead of distroless: the bot re-encodes clips over Telegram's 50 MB
# upload limit with ffmpeg. apk upgrade picks up security fixes released
# after the base image.
FROM arm64v8/alpine:3.24

RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates tzdata ffmpeg \
    && addgroup -g 65532 -S nonroot && adduser -u 65532 -S nonroot -G nonroot

COPY --from=builder /frigate-telegram /frigate-telegram

USER 65532:65532
ENTRYPOINT ["/frigate-telegram"]
