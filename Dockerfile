# Build stage
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/wa-gateway .

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/wa-gateway /app/wa-gateway

ENV STORE_DIR=/app/data \
    PORT=3000
RUN mkdir -p /app/data && chown -R app:app /app
USER app

EXPOSE 3000
VOLUME ["/app/data"]
ENTRYPOINT ["/app/wa-gateway"]
