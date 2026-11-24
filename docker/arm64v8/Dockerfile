FROM arm64v8/golang:1.26.4-alpine AS builder

WORKDIR $GOPATH/frigate-telegram
COPY . $GOPATH/frigate-telegram

RUN apk --no-cache add binutils && \
    go build . && \
    strip frigate-telegram

FROM arm64v8/alpine:3.20

RUN apk --no-cache add ca-certificates tzdata ffmpeg

COPY --from=builder /go/frigate-telegram/frigate-telegram /frigate-telegram

RUN addgroup -g 65532 -S nonroot && adduser -u 65532 -S nonroot -G nonroot
USER 65532:65532

ENTRYPOINT ["/frigate-telegram"]
