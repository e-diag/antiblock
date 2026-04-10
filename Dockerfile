# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Копируем go mod файлы
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Собираем приложение и утилиту перезапуска Premium mtg (CI/CD после деплоя).
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bot ./cmd/bot && \
    CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o restart_premium_mtg ./cmd/restart_premium_mtg

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app

# Бинарник, конфиг и assets (fallback если не используется embed JSON)
COPY --from=builder /app/bot /app/bot
COPY --from=builder /app/restart_premium_mtg /app/restart_premium_mtg
COPY --from=builder /app/config.yaml /app/config.yaml
COPY --from=builder /app/assets /app/assets

ENV TZ=UTC

CMD ["/app/bot"]
