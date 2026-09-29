# syntax=docker/dockerfile:1
# Build stage
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build metadata — supplied by .github/workflows/deploy.yml (or scripts/version-ldflags.sh).
# Defaults keep `docker build` without args working (shows 0.0.0-dev).
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG BUILD_NUMBER=0
ENV LDFLAGS="-s -w \
  -X wa-gateway/pkg/version.Version=${VERSION} \
  -X wa-gateway/pkg/version.Commit=${COMMIT} \
  -X wa-gateway/pkg/version.BuildTime=${BUILD_TIME} \
  -X wa-gateway/pkg/version.BuildNumber=${BUILD_NUMBER}"

RUN CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/wa-gateway . && \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/wagctl ./cmd/wagctl

# Runtime stage
FROM alpine:3.20

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG BUILD_NUMBER=0
LABEL org.opencontainers.image.title="wa-gateway" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.source="https://github.com/FT-Super-Apps/wa-gateway" \
      id.ac.unismuh.lms.build-number="${BUILD_NUMBER}"
ENV APP_VERSION=${VERSION} APP_COMMIT=${COMMIT} APP_BUILD_NUMBER=${BUILD_NUMBER}

RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/wa-gateway /app/wa-gateway
COPY --from=build /out/wagctl     /app/wagctl

ENV STORE_DIR=/app/data \
    PORT=3000
RUN mkdir -p /app/data && chown -R app:app /app
USER app

EXPOSE 3000
VOLUME ["/app/data"]
ENTRYPOINT ["/app/wa-gateway"]
