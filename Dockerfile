FROM golang:1.22-alpine AS build
WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/status-monitor .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/status-monitor /usr/local/bin/status-monitor
COPY config.yaml ./config.yaml
RUN mkdir -p /app/data && chown -R app:app /app
USER app
EXPOSE 8000
# HTTPS: mount your certificates and set server.tls_cert / server.tls_key in config.yaml
VOLUME ["/app/data"]
CMD ["status-monitor", "-config", "config.yaml", "-addr", "0.0.0.0:8000"]
