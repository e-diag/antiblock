## Настройка Premium-сервера и безопасного Docker API

Этот файл описывает, как безопасно поднять **отдельный Premium‑сервер** с Docker и настроить бота так, чтобы он удалённо создавал / удалял / перезапускал MTProto‑прокси для премиум‑юзеров.

Бот НЕ должен крутиться на том же хосте, что Premium‑контейнеры. Вместо этого он подключается к удалённому Docker API c TLS‑аутентификацией.

---

### 1. Архитектура

- **Telegram‑бот** (AntiBlock):
  - крутится на своём сервере;
  - хранит в БД пользователей (`users`) и премиум‑прокси (`proxy_nodes`);
  - при активации/продлении премиума вызывает Docker‑менеджер.

- **Premium‑сервер**:
  - установлены Docker / Docker Engine;
  - запущен Docker‑демон с TCP‑endpoint'ом, защищённым TLS‑сертификатами;
  - доступ к этому API есть только у бота (через client‑cert) и только по закрытому адресу/порту, ограниченному firewall'ом.

Бот при работе использует стандартные переменные `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`, чтобы подключиться к удалённому Docker API.

---

### 2. Что именно делает бот на Premium‑сервере

Бизнес‑логика реализована через:

- `internal/infrastructure/docker/manager.go` — Docker‑менеджер;
- `internal/usecase/user_usecase.go` — управление премиум‑подписками и их связью с Docker;
- `internal/usecase/proxy_usecase.go` — работа с сущностью `ProxyNode`;
- `internal/worker/subscription_worker.go` и `internal/worker/docker_monitor_worker.go` — воркеры.

#### 2.1. Модель `ProxyNode`

```go
type ProxyNode struct {
    ID            uint
    IP            string
    Port          int         // uniqueIndex
    Secret        string
    Type          ProxyType   // "free" или "premium"
    OwnerID       *uint       // владелец персонального премиум‑прокси
    ContainerName string      // имя Docker‑контейнера
    Status        ProxyStatus // active / inactive / blocked
    Load          int
}
```

- Для **персональных премиум‑прокси**:
  - `Type = "premium"`;
  - `OwnerID = user.ID`;
  - `Port` — уникальный порт в диапазоне;
  - `ContainerName = "mtg-user-{tg_id}"`.

#### 2.2. Docker‑менеджер

Файл `internal/infrastructure/docker/manager.go`:

- `NewManager()` — создает клиента Docker, читая `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`.
- `CreateUserContainer(ctx, tgID, proxy *ProxyNode)`:
  - имя контейнера: `mtg-user-{tg_id}`;
  - образ: `p3terx/mtg`;
  - env:
    - `PORT={proxy.Port}`;
    - `SECRET={proxy.Secret}`;
  - `NetworkMode: "host"`;
  - `RestartPolicy: "unless-stopped"`;
  - лимит памяти: **100MB**.
- `RemoveUserContainer(ctx, name)` — остановка и удаление контейнера.
- `IsContainerRunning(ctx, name)` — проверка, запущен ли контейнер.

#### 2.3. При активации / продлении премиума

В `UserUseCase.ActivatePremium(tgID, durationDays)`:

1. Обновляется пользователь:
   - `IsPremium = true`;
   - `PremiumUntil = now + duration`;
   - `LastActiveAt = now`.
2. После успешного сохранения вызывается:

```go
_ = uc.ensurePremiumContainer(tgID, user)
```

`ensurePremiumContainer`:

- Находит или создает персональный премиум‑`ProxyNode` для пользователя (порт, IP, secret, `OwnerID`).
- Формирует имя контейнера `mtg-user-{tg_id}`.
- Проверяет через Docker API, запущен ли контейнер:
  - если да — ничего не делает;
  - если нет — вызывает `CreateUserContainer`, тем самым:
    - при **первом включении** поднимает контейнер,
    - при **продлении**, если контейнер упал или был удалён, поднимает его заново с теми же параметрами.

#### 2.4. Автоматическое выключение и очистка

- `SubscriptionWorker` (раз в N секунд, обычно раз в час):
  - `CheckExpiredPremiums()`:
    - для всех `users` с истекшим `premium_until`:
      - ставит `IsPremium = false`;
      - деактивирует `ProxyNode` (`status = inactive`);
      - удаляет Docker‑контейнер `mtg-user-{tg_id}`.
  - `CleanupExpiredProxies(60)`:
    - ищет премиум‑прокси, у которых подписка истекла более 60 дней назад;
    - удаляет такие `ProxyNode` → освобождает порты;
    - дополнительно удаляет соответствующие Docker‑контейнеры.

#### 2.5. Мониторинг памяти и алерты

`DockerMonitorWorker` (`internal/worker/docker_monitor_worker.go`):

- раз в заданный интервал:
  - находит все контейнеры `mtg-user-*`;
  - читает Docker stats, берет `MemoryStats.Usage`;
  - если usage > 100MB:
    - отправляет сообщение всем `admin_ids` в Telegram:
      - имя контейнера;
      - `tg_id` пользователя;
      - текущий объем памяти.

---

### 3. Подключение к отдельному Docker‑серверу

#### 3.1. Подготовка Premium‑сервера

На отдельном сервере (Linux, root или sudo):

1. Установи Docker:

```bash
curl -fsSL https://get.docker.com | sh
```

2. Сгенерируй TLS‑сертификаты для Docker‑демона и клиента бота.

Можно воспользоваться официальной схемой Docker:

```bash
mkdir -p /etc/docker/certs
cd /etc/docker/certs

# 1) CA
openssl genrsa -out ca-key.pem 4096
openssl req -new -x509 -days 365 -key ca-key.pem -sha256 -subj "/CN=docker-ca" -out ca.pem

# 2) Серверный сертификат (для Docker API)
openssl genrsa -out server-key.pem 4096
openssl req -subj "/CN=premium-docker" -new -key server-key.pem -out server.csr
echo subjectAltName = IP:PREMIUM_SERVER_IP,IP:127.0.0.1 > extfile.cnf
echo extendedKeyUsage = serverAuth >> extfile.cnf
openssl x509 -req -days 365 -sha256 -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out server-cert.pem -extfile extfile.cnf

# 3) Клиентский сертификат (для бота)
openssl genrsa -out key.pem 4096
openssl req -subj "/CN=antiblock-bot" -new -key key.pem -out client.csr
echo extendedKeyUsage = clientAuth > extfile-client.cnf
openssl x509 -req -days 365 -sha256 -in client.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out cert.pem -extfile extfile-client.cnf
```

Получится набор файлов:

- CA: `ca.pem`, `ca-key.pem`;
- сервер: `server-cert.pem`, `server-key.pem`;
- клиент: `cert.pem`, `key.pem`.

3. Перемести **серверные** сертификаты и ключи туда, где их будет читать Docker:

```bash
mkdir -p /etc/docker/tls
cp ca.pem server-cert.pem server-key.pem /etc/docker/tls/
chmod 600 /etc/docker/tls/server-key.pem
```

4. Ограничь доступ к Docker‑порту firewall'ом:

- выбери порт, например `2376`;
- открой его **только** для IP‑адреса сервера, где крутится Telegram‑бот.

Примеры:

- **ufw**:

```bash
ufw allow from BOT_SERVER_IP to any port 2376 proto tcp
ufw deny 2376/tcp
```

- **iptables (минимально)**:

```bash
iptables -A INPUT -p tcp --dport 2376 -s BOT_SERVER_IP -j ACCEPT
iptables -A INPUT -p tcp --dport 2376 -j DROP
```

5. Включи TLS‑порт в конфигурации Docker‑демона.

Открой `/etc/docker/daemon.json` (создай, если нет) и пропиши:

```json
{
  "hosts": [
    "unix:///var/run/docker.sock",
    "tcp://0.0.0.0:2376"
  ],
  "tls": true,
  "tlscacert": "/etc/docker/tls/ca.pem",
  "tlscert": "/etc/docker/tls/server-cert.pem",
  "tlskey": "/etc/docker/tls/server-key.pem",
  "tlsverify": true
}
```

Перезапусти Docker:

```bash
systemctl daemon-reload
systemctl restart docker
```

Проверь, что порт слушается:

```bash
ss -tulpen | grep 2376
```

---

### 4. Настройка клиента на сервере с ботом

На сервере, где крутится AntiBlock:

1. Создай каталог для клиентских сертификатов:

```bash
mkdir -p /opt/antiblock/docker-certs
```

2. Скопируй из Premium‑сервера **только** следующие файлы:

- `ca.pem`
- `cert.pem`
- `key.pem`

и положи их в `/opt/antiblock/docker-certs`.

3. Настрой переменные окружения для контейнера с ботом (например, через `docker-compose.yml` или `env`):

- `DOCKER_HOST=tcp://PREMIUM_SERVER_IP:2376`
- `DOCKER_TLS_VERIFY=1`
- `DOCKER_CERT_PATH=/opt/antiblock/docker-certs`

Пример для `docker-compose.yml` (если ты запускаешь бота через compose):

```yaml
services:
  antiblock-bot:
    image: your/antiblock:latest
    env_file:
      - .env
    environment:
      - DOCKER_HOST=tcp://PREMIUM_SERVER_IP:2376
      - DOCKER_TLS_VERIFY=1
      - DOCKER_CERT_PATH=/opt/antiblock/docker-certs
    volumes:
      - /opt/antiblock/docker-certs:/opt/antiblock/docker-certs:ro
```

Где в `.env` — твои обычные переменные (БД, токен бота и т.п.).

4. Убедись, что из контейнера с ботом Docker API доступен:

Внутри контейнера:

```bash
apk add curl # (или apt-get update && apt-get install -y curl)
curl https://PREMIUM_SERVER_IP:2376/_ping \
  --cacert /opt/antiblock/docker-certs/ca.pem \
  --cert /opt/antiblock/docker-certs/cert.pem \
  --key /opt/antiblock/docker-certs/key.pem
```

Если всё ок, увидишь `OK`.

---

### 5. Безопасность

Чтобы не превратить Docker API в дыру:

- **Никогда** не открывай Docker на `0.0.0.0:2375` **без TLS**.
- Используй только `2376` с `tlsverify=true` и собственным CA.
- Ограничь доступ по IP‑адресу бота (firewall).
- Храни клиентские ключи (`key.pem`) только на сервере с ботом, с правами `600`.
- Не логи содержимое ключей и не коммить их в репозиторий.
- Регулярно перевыпускай сертификаты (раз в 6–12 месяцев).

---

### 6. Как это увязано с кодом

1. При старте бота в `cmd/bot/main.go`:
   - создаётся `dockerMgr := docker.NewManager()`;
   - `userUC := NewUserUseCase(userRepo, proxyRepo, dockerMgr)`;
   - `botHandler := NewBotHandler(..., dockerMgr, ...)`;
   - запускается `DockerMonitorWorker` с тем же `*bot.Bot`.

2. При выдаче / продлении премиума (CryptoPay, Telegram Stars, `/grantpremium`) → `ActivatePremium`:
   - создаёт/обновляет персональный `ProxyNode`;
   - гарантирует наличие контейнера `mtg-user-{tg_id}`.

3. Воркеры:
   - `SubscriptionWorker` — выключает/очищает истекшие премиумы и их контейнеры;
   - `DockerMonitorWorker` — следит за 100MB лимитом и шлёт алерты.

4. Админ‑команды:
   - `/admin_stats` — показывает число активных премиум‑контейнеров, занятые порты и примерное количество свободных слотов по памяти;
   - `/admin_info {tg_id}` — показывает состояние прокси и Docker‑контейнера юзера;
   - `/admin_rebuild {tg_id}` — форсирует пересоздание контейнера для указанного `tg_id`.

Таким образом бот полностью управляет жизненным циклом персональных премиум‑прокси на **отдельном Docker‑сервере**, оставаясь при этом изолированным и не давая прямого доступа к Docker API наружу.

---

### 7. Логирование контейнеров mtg на премиум‑сервере

Бот при создании контейнеров задаёт для них **LogConfig** (json-file, max-size: 1m, max-file: 1), чтобы логи mtg не разрастались. Вывести «только ошибки» на уровне контейнера нельзя — это зависит от самого бинарника mtg. Чтобы уменьшить шум в логах на премиум‑сервере, можно:

- Настроить драйвер логирования демона или контейнеров (например, в `daemon.json` или через `docker run --log-driver`).
- Ограничить уровень логирования в mtg, если в образе или конфиге такая опция есть.

