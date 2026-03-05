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

### Перенос базы данных на отдельный сервис

Бот не зависит от локального контейнера PostgreSQL и подключается к БД по параметрам из `config.yaml` или переменных окружения (`DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`, `DB_SSLMODE`, `DB_TIMEZONE`).
Это позволяет вынести Postgres в отдельный сервис (другой сервер, managed‑БД, кластер и т.п.).

**Шаги переноса:**

1. **Подготовьте новый инстанс PostgreSQL.**
   - Поднимите отдельный PostgreSQL (контейнер/VM/managed‑сервис).
   - Создайте пустую базу данных, например:
     ```bash
     createdb antiblock
     ```

2. **Остановите бота, чтобы зафиксировать состояние данных.**
   - Остановите контейнер `antiblock-bot` или процесс `go run cmd/bot/main.go`.

3. **Сделайте дамп текущей БД и восстановите его на новом сервисе.**
   - Выполните `pg_dump` из старой БД (из контейнера или с хоста, где она запущена):
     ```bash
     pg_dump -h <старый_host> -p <старый_port> -U <user> -Fc -d antiblock -f antiblock.dump
     ```
   - Восстановите дамп в новую БД:
     ```bash
     pg_restore -h <новый_host> -p <новый_port> -U <user> -d antiblock --clean --if-exists antiblock.dump
     ```

4. **Обновите настройки подключения к БД для бота.**
   - Если запускаете через бинарь/`go run` — измените блок `database` в `config.yaml`:
     ```yaml
     database:
       host: "NEW_DB_HOST"
       port: "5432"
       user: "postgres"
       password: "your_password"
       dbname: "antiblock"
       sslmode: "disable"
       timezone: "UTC"
     ```
   - Если запускаете через Docker Compose — измените переменные окружения для сервиса `bot` в `docker-compose.yml` или `.env`:
     ```yaml
     environment:
       DB_HOST: new-db-host.example.com
       DB_PORT: 5432
       DB_USER: ${DB_USER:-postgres}
       DB_PASSWORD: ${DB_PASSWORD:-postgres}
       DB_NAME: ${DB_NAME:-antiblock}
       DB_SSLMODE: disable
       DB_TIMEZONE: UTC
     ```

5. **Запустите бота с новыми настройками.**
   - При первом подключении GORM выполнит `AutoMigrate`, а функция `runMigrations` донастроит данные (типы/статусы прокси и т.п.) без потери существующих записей.

6. **Проверьте работу.**
   - Убедитесь, что бот стартует без ошибок подключения к БД.
   - Проверьте выдачу прокси, статистику и фоновые воркеры.

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

## ✅ Что и как тестировать

- **Миграция БД на отдельный сервис**:
  - Проверить, что после переноса (по инструкции выше) бот успешно подключается к новой БД: нет ошибок в логах, все команды работают.
  - Убедиться, что данные (пользователи, прокси, объявления, платежи) сохранены после `pg_dump` / `pg_restore`.
- **Объявления и статистика**:
  - Создать активное объявление, получить его в боте как обычный пользователь.
  - Нажать на кнопку объявления: в админ‑панели в статистике должны увеличиться `Клики`, а `Показы` — при каждой отправке объявления.
  - Открепить объявление в чате руками и дождаться, пока воркер `ad_repin` снова закрепит его.
- **Оплата Premium**:
  - Проверить, что в меню покупки премиума отображаются корректные цены из настроек (`premium_usdt`, `premium_stars`).
  - Для Stars: пройти полный сценарий оплаты через Telegram Stars и убедиться, что премиум активируется.
  - Для USDT/xRocket: создать счёт, перейти по ссылке оплаты, убедиться, что по webhook от xRocket премиум активируется, а запись счёта помечается как `paid`.

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
