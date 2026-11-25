FROM arm32v6/golang:1.27.4-alpine3.24 AS builder

WORKDIR /src

# Download modules in their own layer, so code changes don't download them
# again (slow on emulated ARM builds).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary; -s -w strips symbols and debug info like the old strip step.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /frigate-telegram .

FROM arm32v6/alpine

RUN apk --no-cache add tzdata

COPY --from=builder /frigate-telegram /frigate-telegram

USER nobody

ENTRYPOINT ["/frigate-telegram"]
