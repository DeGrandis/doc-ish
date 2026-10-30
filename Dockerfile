# syntax=docker/dockerfile:1
# Multi-arch (amd64 and arm64): Go runs on the build machine's own platform
# and cross-compiles for the target, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# The version the app reports: a release tag, git describe, or "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/mymedical ./cmd/mymedical
RUN mkdir -p /out/data

# On Debian slim: Poppler reads and renders PDFs for the file reader,
# Tesseract reads the print on scanned pages (English), and GDCM's
# gdcmconv decompresses the DICOM images Go can't read itself (JPEG
# Lossless, JPEG-LS, JPEG 2000, RLE), which Alpine doesn't package.
FROM debian:trixie-slim AS app
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates poppler-utils libgdcm-tools tesseract-ocr \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 65532 --no-create-home --shell /usr/sbin/nologin app
COPY --from=build /out/mymedical /mymedical
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV MYMEDICAL_ADDR=0.0.0.0:8080 MYMEDICAL_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/mymedical"]
