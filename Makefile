.PHONY: build run test clean docker-build docker-up docker-down

# Переменные
BINARY_NAME=bot
DOCKER_COMPOSE=docker-compose

# Сборка приложения
build:
	go build -o $(BINARY_NAME) ./cmd/bot

# Запуск приложения
run:
	go run ./cmd/bot

# Запуск тестов
test:
	go test ./...

# Очистка
clean:
	go clean
	rm -f $(BINARY_NAME)

# Установка зависимостей
deps:
	go mod download
	go mod tidy

# Docker команды
docker-build:
	$(DOCKER_COMPOSE) build

docker-up:
	$(DOCKER_COMPOSE) up -d

docker-down:
	$(DOCKER_COMPOSE) down

docker-logs:
	$(DOCKER_COMPOSE) logs -f bot

# Полный перезапуск
docker-restart: docker-down docker-build docker-up
