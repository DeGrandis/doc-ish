# The dev server's base: Go plus the tools the app image has (Poppler for
# PDFs, GDCM for compressed DICOM). Build once:
#   docker build -t mymedical-devbase -f dev.Dockerfile .
FROM golang:1.26
RUN apt-get update \
 && apt-get install -y --no-install-recommends poppler-utils libgdcm-tools \
 && rm -rf /var/lib/apt/lists/*
