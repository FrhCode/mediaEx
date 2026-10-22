FROM node:24-alpine AS frontend
WORKDIR /build/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build && rm -rf dist/media dist/mockServiceWorker.js

FROM golang:1.27-alpine AS backend
WORKDIR /build/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mediaex ./cmd/server

FROM backend AS backend-test
RUN apk add --no-cache ffmpeg
RUN go test ./... && go vet ./...

FROM alpine:3.23 AS backend-runtime
RUN apk add --no-cache ffmpeg ca-certificates wget \
    && addgroup -g 10001 mediaex \
    && adduser -D -u 10001 -G mediaex mediaex \
    && mkdir -p /data /media /app/web \
    && chown mediaex:mediaex /data
COPY --from=backend /out/mediaex /app/mediaex
ENV LISTEN_ADDR=:8080 MEDIA_ROOT=/media DATA_DIR=/data STATIC_DIR=/app/web
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/mediaex"]

FROM backend-runtime AS runtime
COPY --from=frontend /build/frontend/dist /app/web
COPY docs/openapi.yaml /app/web/docs/openapi.yaml
