FROM golang:1.26.1-alpine3.22 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/moe-asset-client \
    -trimpath \
    -tags netgo \
    ./cmd/client

FROM mcr.microsoft.com/dotnet/sdk:9.0-bookworm-slim AS assetstudio-builder
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && \
    rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --single-branch --branch sekai-modify https://github.com/Team-Haruki/AssetStudio.git
RUN cd AssetStudio/AssetStudioCLI && \
    dotnet publish -c Release -r linux-x64 -f net9.0 --self-contained true -o /app/assetstudio \
    -p:PublishTrimmed=false \
    -p:PublishSingleFile=true \
    -p:IncludeNativeLibrariesForSelfExtract=true

FROM mwader/static-ffmpeg:7.1.1 AS ffmpeg-builder

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    libicu76 \
    libxml2 && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/moe-asset-client /app/moe-asset-client
COPY --from=assetstudio-builder /app/assetstudio /app/assetstudio
COPY --from=ffmpeg-builder /ffmpeg /usr/local/bin/ffmpeg
COPY config.example.yaml /app/config.example.yaml
RUN ln -sf /app/assetstudio/AssetStudioModCLI /app/assetstudio/AssetStudioCLI && \
    mkdir -p /app/work
ENV TZ=Asia/Shanghai \
    DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=false \
    MOE_ASSET_CLIENT_CONFIG=/app/config.yaml
CMD ["/app/moe-asset-client", "-config", "/app/config.yaml"]
