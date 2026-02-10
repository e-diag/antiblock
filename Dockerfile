# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Копируем go mod файлы
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код
COPY . .

# Собираем приложение
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bot ./cmd/bot

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata
WORKDIR /root/

# Копируем бинарник из builder stage
COPY --from=builder /app/bot .
COPY --from=builder /app/config.yaml .

# Устанавливаем переменные окружения по умолчанию
ENV TZ=UTC

# Запускаем приложение
CMD ["./bot"]
