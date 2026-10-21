# Garner's container image: the server, garnerd, plus an FFprobe that can only read
# the files Garner imports. Build with `mise run image`.
#
# Base images are pinned by index digest and FFmpeg by checksum; update them
# deliberately. The image runs as UID 1000 on a read-only root filesystem.

ARG ALPINE=docker.io/library/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG GOLANG=docker.io/library/golang:1.27.3-alpine3.24@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd

# A static FFprobe built from the FFmpeg release, with only the container
# readers and stream parsers the file check needs: no decoders, no network
# and no devices. Less code faces untrusted downloads, and the binary is a
# few MB instead of a full FFmpeg. Without --enable-gpl, this build is under
# the LGPL 2.1 or later, with zlib linked in under the zlib license. The
# LGPL text ships in the image; the source is the release named below,
# built with these options.
FROM ${ALPINE} AS ffprobe
RUN apk add --no-cache build-base xz zlib-dev zlib-static
WORKDIR /src
# Signed by the FFmpeg release key FCF986EA15E6E293A5644F10B4322F04D67658D8;
# the signature was checked when this checksum was pinned.
ADD --checksum=sha256:8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e \
    https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz /ffmpeg.tar.xz
RUN tar -xJf /ffmpeg.tar.xz --strip-components=1 \
 && ./configure \
      --disable-everything --disable-autodetect --disable-programs --enable-ffprobe \
      --disable-doc --disable-debug --disable-network --disable-asm \
      --disable-avdevice --disable-avfilter --disable-swscale --disable-swresample \
      --enable-small --enable-static --disable-shared --extra-ldflags=-static \
      --enable-zlib --enable-protocol=file \
      --enable-demuxer=matroska,mov,avi,mpegts,asf \
      --enable-parser=h264,hevc,av1,vp8,vp9,mpeg4video,mpegvideo,vc1,aac,ac3,dca,mlp,mpegaudio,opus,vorbis,flac \
 && make -j"$(nproc)" \
 && strip ffprobe

FROM ${GOLANG} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
ARG VERSION=dev
ARG COMMIT=
# musl selects Turso's library for Alpine; timetzdata embeds time zones.
RUN CGO_ENABLED=0 go build -tags musl,timetzdata -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/garnerd ./cmd/garnerd

FROM ${ALPINE}
ARG VERSION=dev
ARG COMMIT=
# The source label links the image on ghcr.io to the repository.
LABEL org.opencontainers.image.title="Garner" \
      org.opencontainers.image.source="https://github.com/trucke/garner" \
      org.opencontainers.image.licenses="MIT AND LGPL-2.1-or-later AND Zlib" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
# Turso's library needs libgcc_s.
RUN apk add --no-cache libgcc
COPY --from=ffprobe /src/ffprobe /usr/local/bin/ffprobe
COPY --from=ffprobe /src/COPYING.LGPLv2.1 /usr/share/licenses/ffprobe/COPYING.LGPLv2.1
COPY LICENSE /usr/share/licenses/garner/LICENSE
COPY --from=build /out/garnerd /usr/local/bin/garnerd
# garnerd unpacks Turso's library into the data directory on first start.
ENV GARNER_CONFIG=/config/garner.toml
USER 1000:1000
WORKDIR /data
EXPOSE 7979
ENTRYPOINT ["/usr/local/bin/garnerd"]
CMD ["serve"]
