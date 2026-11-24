FROM golang:1.27.4-alpine3.24 AS builder

WORKDIR /go/frigate-telegram

RUN apk --no-cache add binutils

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build . && \
    strip frigate-telegram

FROM alpine:3.24

RUN apk update && apk upgrade --no-cache && \
    apk add --no-cache ca-certificates tzdata ffmpeg

COPY --from=builder /go/frigate-telegram/frigate-telegram /frigate-telegram

RUN addgroup -g 65532 -S nonroot && adduser -u 65532 -S nonroot -G nonroot
USER 65532:65532

ENTRYPOINT ["/frigate-telegram"]
