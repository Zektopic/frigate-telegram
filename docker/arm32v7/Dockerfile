FROM arm32v7/golang:1.27.4-alpine3.24 AS builder

WORKDIR /src

# Download modules in their own layer, so code changes don't download them
# again (slow on emulated ARM builds).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary; -s -w strips symbols and debug info like the old strip step.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /frigate-telegram .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /frigate-telegram /frigate-telegram

ENTRYPOINT ["/frigate-telegram"]
