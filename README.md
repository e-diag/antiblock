# AntiBlock MTProto Proxy Bot

Высокопроизводительный Telegram бот для управления MTProto Proxy сервисом с поддержкой премиум подписок через CryptoPay.

## 🏗️ Архитектура

Проект использует Clean Architecture с разделением на слои:
- **Domain** - бизнес-сущности (User, ProxyNode, Ad)
- **Repository** - слой доступа к данным
- **UseCase** - бизнес-логика
- **Handler** - обработчики Telegram команд
- **Infrastructure** - конфигурация, база данных
- **Worker** - фоновые задачи

## 🚀 Быстрый старт

### Требования

- Go 1.21+
- PostgreSQL 15+
- Docker и Docker Compose (опционально)

### Установка

1. Клонируйте репозиторий:
```bash
git clone <repository-url>
cd AntiBlock
```

2. Установите зависимости:
```bash
go mod download
```

3. Настройте переменные окружения в `config.yaml` или через `.env`:

**Получение Telegram Bot Token:**
- Откройте [@BotFather](https://t.me/BotFather) в Telegram
- Отправьте команду `/newbot` и следуйте инструкциям
- Скопируйте полученный токен

**Получение Telegram User ID (для админа):**
- Откройте [@userinfobot](https://t.me/userinfobot) в Telegram
- Скопируйте ваш ID

**Получение CryptoBot API Token:**
- Зарегистрируйтесь на [CryptoPay](https://crypt.bot/)
- Создайте бота и получите API токен

```yaml
telegram:
  bot_token: "YOUR_BOT_TOKEN"
  admin_ids:
    - "YOUR_ADMIN_TELEGRAM_ID"

cryptobot:
  api_token: "YOUR_CRYPTOBOT_API_TOKEN"
```

4. Запустите PostgreSQL и создайте базу данных:
```bash
createdb antiblock
```

5. Запустите бота:
```bash
go run cmd/bot/main.go
```

### Docker Compose

1. Создайте файл `.env` с переменными окружения:
```env
TELEGRAM_BOT_TOKEN=your_bot_token
TELEGRAM_ADMIN_ID_1=your_admin_id
CRYPTOBOT_API_TOKEN=your_cryptobot_token
DB_PASSWORD=your_db_password
```

2. Запустите через Docker Compose:
```bash
docker-compose up -d
```

## 📋 Функционал

### Для пользователей

- `/start` - Приветственное сообщение с кнопками
- Кнопка "Получить прокси" - выдает доступный прокси-сервер
- Кнопка "Купить премиум" - создает счет на оплату через CryptoPay

### Для администраторов

- `/addproxy <ip> <port> <secret> <type>` - Добавить прокси-узел (Free/Premium)
- `/stats` - Статистика по пользователям и прокси
- `/broadcast` - Рассылка сообщений всем пользователям

## 🔧 Конфигурация

Основные настройки в `config.yaml`:

- **Rate Limiting**: Ограничение запросов (по умолчанию 1 запрос/сек)
- **Workers**: Интервалы проверки здоровья прокси и подписок
- **Database**: Параметры подключения к PostgreSQL

## 🔒 Безопасность

- Rate limiting middleware (1 запрос/сек с burst до 3)
- Валидация всех входных данных
- Проверка прав администратора для админ-команд
- Защита от SQL-инъекций через GORM

## 📊 Фоновые задачи

1. **Health Check Worker** - Проверяет доступность прокси каждые 5 минут
2. **Subscription Checker** - Проверяет истечение премиум подписок каждую минуту

## 🛠️ Разработка

### Структура проекта

```
.
├── cmd/bot/          # Точка входа приложения
├── internal/
│   ├── domain/       # Бизнес-сущности
│   ├── repository/   # Слой доступа к данным
│   ├── usecase/      # Бизнес-логика
│   ├── handler/      # Обработчики Telegram
│   ├── infrastructure/ # Конфигурация и БД
│   └── worker/       # Фоновые задачи
├── config.yaml       # Конфигурация
├── Dockerfile
└── docker-compose.yml
```

### Добавление нового прокси

```bash
/addproxy 1.2.3.4 443 dd1234567890abcdef1234567890abcdef Free
```

## 📝 Лицензия

MIT
