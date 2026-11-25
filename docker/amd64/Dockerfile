FROM golang:1.27.4-alpine3.24 AS builder

WORKDIR /src

# Download modules in their own layer, so code changes don't download them
# again (slow on emulated ARM builds).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary; -s -w strips symbols and debug info like the old strip step.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /frigate-telegram .

# Minimal static ffmpeg for re-encoding clips over Telegram's 50 MB upload
# limit. Only what Frigate clips need is compiled in: MP4 with H.264/H.265 video
# and AAC/PCM audio, the scale filter, and the x264 and AAC encoders. Built
# from the 9.1 release, which fixes the 8.x deband/dynaudnorm filter CVEs
# (CVE-2026-135736, CVE-2026-135743); those filters, the zmq/sendcmd command
# interfaces and networking aren't compiled in at all. Static, so the runtime
# stays distroless.
FROM alpine:3.24 AS ffmpeg-builder
ARG FFMPEG_VERSION=n9.1
ARG X264_COMMIT=b35605ace3ddf7c1a5d67a2eb553f034aef41d55
RUN apk add --no-cache bash build-base git nasm pkgconf linux-headers
RUN git clone https://code.videolan.org/videolan/x264.git /src/x264 \
    && cd /src/x264 && git checkout -q "$X264_COMMIT" \
    && ./configure --prefix=/opt/ffmpeg --enable-static --disable-cli --disable-opencl --enable-pic \
    && make -j"$(nproc)" && make install
RUN git clone --depth 1 --branch "$FFMPEG_VERSION" https://github.com/FFmpeg/FFmpeg.git /src/ffmpeg \
    && cd /src/ffmpeg \
    && PKG_CONFIG_PATH=/opt/ffmpeg/lib/pkgconfig ./configure \
        --prefix=/opt/ffmpeg --pkg-config-flags=--static \
        --extra-cflags=-I/opt/ffmpeg/include --extra-ldflags="-L/opt/ffmpeg/lib -static" \
        --enable-gpl --enable-libx264 --enable-static --disable-shared \
        --disable-autodetect --disable-debug --disable-doc --disable-network \
        --disable-ffplay --disable-ffprobe --disable-everything \
        --enable-protocol=file,pipe \
        --enable-demuxer=mov --enable-muxer=mp4 \
        --enable-decoder=h264,hevc,aac,aac_latm,mp3,opus,pcm_alaw,pcm_mulaw,pcm_s16le \
        --enable-parser=h264,hevc,aac \
        --enable-bsf=aac_adtstoasc,h264_mp4toannexb,hevc_mp4toannexb \
        --enable-encoder=libx264,aac \
        --enable-filter=buffer,buffersink,abuffer,abuffersink,scale,format,null,anull,aformat,aresample \
        --enable-swscale --enable-swresample \
    && make -j"$(nproc)" && make install \
    && strip /opt/ffmpeg/bin/ffmpeg

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /frigate-telegram /frigate-telegram
COPY --from=ffmpeg-builder /opt/ffmpeg/bin/ffmpeg /usr/local/bin/ffmpeg

ENTRYPOINT ["/frigate-telegram"]
