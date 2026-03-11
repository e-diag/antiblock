# Подключение Prometheus и Grafana (AntiBlock Bot)

Инструкция по запуску стека мониторинга и доступу к Grafana по адресу **https://notecalendar.ru/grafana**.

## Что уже настроено

- **Бот** отдаёт метрики на порту `9090` по пути `/metrics` (см. `cmd/bot/main.go`).
- **Prometheus** собирает метрики с цели `bot:9090` каждые 30 секунд.
- **Grafana** настроена на работу по подпути: `https://notecalendar.ru/grafana`.
- Источник данных Prometheus в Grafana создаётся автоматически (provisioning).

## Метрики бота

| Метрика | Тип | Описание |
|--------|-----|----------|
| `antiblock_free_proxies_active` | Gauge | Количество активных free-прокси |
| `antiblock_free_proxies_inactive` | Gauge | Количество неактивных free-прокси |
| `antiblock_premium_proxies_active` | Gauge | Количество активных premium-прокси |
| `antiblock_premium_proxies_unreachable` | Gauge | Количество недоступных premium-прокси |
| `antiblock_users_total` | Gauge | Всего пользователей |
| `antiblock_users_premium` | Gauge | Пользователей с активным premium |
| `antiblock_free_proxy_load` | Gauge (labels: ip, port) | Нагрузка (число пользователей) на каждый free-прокси |

Дополнительно доступны стандартные метрики Go (runtime, память и т.д.).

---

## Откуда берутся данные (важно)

**Prometheus не читает вашу БД (PostgreSQL).** Цепочка такая:

1. **Бот** раз в 30 секунд запрашивает данные из БД (число пользователей, прокси и т.д.) и выставляет текущие значения метрик в памяти. По запросу отдаёт их по HTTP на `:9090/metrics`.
2. **Prometheus** периодически (каждые 30 с) обращается к боту по HTTP (`GET http://bot:9090/metrics`), забирает снимок метрик и сохраняет его в **своё** хранилище временных рядов (TSDB).
3. **Grafana** рисует графики, запрашивая данные у Prometheus.

Итого: **история в Grafana есть только с момента, когда вы запустили Prometheus и он начал скрейпить бота.** Данных «с самого первого запуска бота» в Prometheus нет — он не подключался к приложению до своего старта. Срок хранения истории в Prometheus задаётся параметром `--storage.tsdb.retention.time=30d` (в `docker-compose.monitoring.yml` — 30 дней).

**Данные по пользователям с первого дня:** в Grafana добавлен источник **PostgreSQL** (та же БД, что и у бота). Панели на основе SQL-запросов к таблице `users` показывают всю накопленную историю (раздел 4.1).

---

## 1. Запуск основного приложения

Убедитесь, что бот и БД запущены и создают сеть Docker:

```bash
docker compose up -d
# или
docker-compose up -d
```

Проверка метрик локально:

```bash
curl -s http://127.0.0.1:9090/metrics | head -30
```

---

## 2. Запуск Prometheus и Grafana

Из корня проекта:

```bash
docker compose -f docker-compose.monitoring.yml up -d
# или
docker-compose -f docker-compose.monitoring.yml up -d
```

После запуска:

- **Prometheus UI**: http://127.0.0.1:9091 (только localhost)
- **Grafana UI**: http://127.0.0.1:3000 (локально; снаружи — через nginx по https://notecalendar.ru/grafana)

Пароль админа Grafana задаётся переменной `GRAFANA_PASSWORD` (по умолчанию `admin`). При первом входе смените его.

---

## 3. Доступ по домену notecalendar.ru

Grafana уже настроена на работу по подпути: `GF_SERVER_ROOT_URL=https://notecalendar.ru/grafana` и `GF_SERVER_SERVE_FROM_SUB_PATH=true`.

### 3.1. Nginx: проксирование /grafana

Добавьте в конфиг виртуального хоста для `notecalendar.ru`:

```nginx
# В server { ... } для notecalendar.ru

location /grafana/ {
    proxy_pass http://127.0.0.1:3000/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Scheme $scheme;
}
```

Перезагрузите nginx и откройте: **https://notecalendar.ru/grafana/**

(Важно: в `proxy_pass` указать слэш в конце `http://127.0.0.1:3000/`, в `location` — `/grafana/`.)

### 3.2. (Опционально) Prometheus по подпути

Если нужно открыть Prometheus снаружи по тому же домену (например, https://notecalendar.ru/prometheus/):

```nginx
location /prometheus/ {
    proxy_pass http://127.0.0.1:9091/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Prometheus по умолчанию не рассчитан на работу под подпутём; при необходимости можно настроить `--web.route-prefix` и `--web.external-url`.

---

## 4. Первый вход в Grafana

1. Откройте **https://notecalendar.ru/grafana/** (или http://127.0.0.1:3000).
2. Логин: `admin`. Пароль: значение `GRAFANA_PASSWORD` или `admin` по умолчанию.
3. Смените пароль при первом входе.
4. Источник данных **Prometheus** уже добавлен (URL: `http://prometheus:9090`). В Explore можно проверить запросы, например:
   - `antiblock_users_total`
   - `antiblock_premium_proxies_active`
5. Источник **PostgreSQL** добавлен автоматически (хост `postgres`, БД `antiblock`, пароль из `DB_PASSWORD` в .env). Он нужен для графиков **по данным с первого дня** (см. раздел 4.1).

---

## 4.1. Данные по пользователям с первого дня (PostgreSQL)

Чтобы видеть статистику пользователей **за всё время** (с момента первого запуска бота), используйте в Grafana источник данных **PostgreSQL** — он подключается к той же БД, что и бот. Данные в таблице `users` не удаляются, поэтому по ним можно строить отчёты за любой период.

**Откуда берётся источник:** Grafana подхватывает PostgreSQL из файла `monitoring/grafana/provisioning/datasources/postgres.yml` (хост `postgres:5432`, БД `antiblock`, пользователь `postgres`, пароль в файле по умолчанию `postgres`, `sslmode: disable`). Если в вашей БД другой пароль — откройте в Grafana **Connections → Data sources → PostgreSQL**, введите правильный пароль (как в `.env` — `DB_PASSWORD`), нажмите **Save & test**.

### Проверка подключения PostgreSQL (нет данных / ошибка подключения)

1. **Сначала запустите основной стек** (чтобы создалась сеть и контейнер `postgres`):
   ```bash
   docker compose up -d
   ```
2. **Потом запустите мониторинг** (Grafana должна быть в той же сети, что и Postgres):
   ```bash
   docker compose -f docker-compose.monitoring.yml up -d
   ```
3. **Имя сети:** в `docker-compose.monitoring.yml` указано `name: antiblock_antiblock-network`. Оно должно совпадать с сетью основного проекта. Узнать имя сети после `docker compose up -d`:
   ```bash
   docker network ls | findstr antiblock
   ```
   Если имя другое (например, из-за другого имени папки), в `docker-compose.monitoring.yml` в секции `networks` замените `name:` на фактическое.
4. **В Grafana:** **☰ → Connections → Data sources → PostgreSQL**. Нажмите **Save & test**. Должно появиться зелёное «Database connection successful». Если ошибка:
   - **Connection refused / no route to host** — Grafana не видит хост `postgres`. Проверьте, что оба контейнера в одной сети: `docker network inspect antiblock_antiblock-network` (или ваше имя) и что там есть контейнеры `antiblock-bot`, `antiblock-postgres`, `antiblock-grafana`.
   - **password authentication failed** — в форме укажите пароль из вашего `.env` (`DB_PASSWORD`), нажмите **Save & test**.
5. **Логи Grafana** (если нужно смотреть ошибки):
   ```bash
   docker logs antiblock-grafana
   ```
   После исправления пароля или сети перезапустите Grafana: `docker restart antiblock-grafana`.

**Примеры панелей на SQL (источник — PostgreSQL):**

---

**Один график: все пользователи и Premium по дням (рекомендуется)**

Удобно смотреть рост базы и долю премиум на одном графике.

- Data source: **PostgreSQL**.
- Режим: **Code** (SQL).
- Один запрос с двумя метриками (две колонки значений):

```sql
WITH days AS (
  SELECT date_trunc('day', d) AS day
  FROM generate_series(
    (SELECT COALESCE(MIN(created_at), NOW()) FROM users),
    NOW(),
    '1 day'::interval
  ) d
)
SELECT
  days.day AS time,
  (SELECT COUNT(*) FROM users WHERE created_at <= days.day + interval '1 day') AS "Всего пользователей",
  (SELECT COUNT(*) FROM users WHERE premium_until IS NOT NULL AND premium_until > days.day) AS "Premium пользователей"
FROM days
ORDER BY 1;
```

- В настройках запроса (Query options): **Format as** → **Time series**.
- В **Transform** добавьте **Convert field type**: колонку `time` — к типу **Time**. Если Grafana сама подхватила время — можно не менять.
- В визуализации укажите: **Time** = `time`, значения — колонки `Всего пользователей` и `Premium пользователей` (должны появиться две линии).
- Visualization: **Time series**. Заголовок: `Пользователи и Premium по дням`.
- Сохраните панель (**Apply**), затем дашборд.

Итог: на одном графике две линии — накопительный итог всех пользователей и число пользователей с активным Premium на каждый день (по данным с первого дня).

---

**Всего пользователей за всё время**

- Data source: **PostgreSQL**.
- Режим запроса: **Code** (или **Builder**).
- SQL:
```sql
SELECT COUNT(*) AS "users" FROM users;
```
- Visualization: **Stat**. Заголовок: `Всего пользователей (с первого дня)`.

---

**Новые пользователи по дням (график с первого дня)**

- Data source: **PostgreSQL**.
- SQL (результат должен содержать колонки с временной меткой и числом):
```sql
SELECT
  date_trunc('day', created_at AT TIME ZONE 'UTC') AS time,
  COUNT(*) AS "новых пользователей"
FROM users
GROUP BY 1
ORDER BY 1;
```
- В панели в настройках запроса включите **Format as**: Time series; колонка времени — `time`, значение — `новых пользователей`.
- Visualization: **Time series**. Заголовок: `Регистрации по дням`.

---

**Накопительный итог пользователей по дням**

- SQL:
```sql
SELECT
  date_trunc('day', created_at AT TIME ZONE 'UTC') AS time,
  COUNT(*) OVER (ORDER BY date_trunc('day', created_at AT TIME ZONE 'UTC')) AS "всего пользователей"
FROM users
GROUP BY date_trunc('day', created_at AT TIME ZONE 'UTC')
ORDER BY 1;
```
- Format as: Time series. Visualization: **Time series**. Заголовок: `Рост числа пользователей (накопительно)`.

---

**Сейчас с активным Premium**

- SQL:
```sql
SELECT COUNT(*) AS "premium"
FROM users
WHERE is_premium = true AND premium_until > NOW();
```
- Visualization: **Stat**. Заголовок: `Premium (активных сейчас)`.

---

**Таблица: последние зарегистрированные**

- SQL:
```sql
SELECT id, tg_id, username, created_at
FROM users
ORDER BY created_at DESC
LIMIT 20;
```
- Visualization: **Table**. Заголовок: `Последние 20 пользователей`.

---

Сохраните дашборд. Эти панели показывают данные из БД за весь период, а не только с момента запуска Prometheus.

---

## 5. Настройка графиков в Grafana

### 5.1. Создание дашборда

1. В левом меню: **☰ → Dashboards** (или иконка «четыре квадрата»).
2. Нажмите **New** → **New dashboard**.
3. Нажмите **Add visualization** (или **Add** → **Visualization**).

### 5.2. Добавление панелей по шагам

Для каждой панели: выберите источник данных **Prometheus** и в поле запроса (Query) введите метрику как есть.

---

**Панель 1 — Всего пользователей (большая цифра)**

- Data source: **Prometheus**.
- В блоке **Query**: в поле A введите `antiblock_users_total`.
- Справа в **Visualization** выберите **Stat**.
- В **Panel options** → Title: `Всего пользователей`.
- Нажмите **Apply** (или **Save**), затем **Save dashboard** (назовите дашборд, например «AntiBlock»).

---

**Панель 2 — Premium пользователи**

- **Add** → **Visualization** (или дублируйте панель и измените запрос).
- Запрос: `antiblock_users_premium`.
- Visualization: **Stat**.
- Title: `Premium пользователи`.

---

**Панель 3 — Free-прокси (активные / неактивные)**

Две метрики в одной панели (график по времени):

- Запрос A: `antiblock_free_proxies_active` → в **Legend** задайте `Active`.
- Нажмите **+ Query** и добавьте запрос B: `antiblock_free_proxies_inactive` → Legend: `Inactive`.
- Visualization: **Time series** (график).
- Title: `Free-прокси: активные и неактивные`.

---

**Панель 4 — Premium-прокси**

- Запрос A: `antiblock_premium_proxies_active`, Legend: `Active`.
- Запрос B: `antiblock_premium_proxies_unreachable`, Legend: `Unreachable`.
- Visualization: **Time series**.
- Title: `Premium-прокси`.

---

**Панель 5 — Нагрузка по каждому free-прокси**

- Запрос: `antiblock_free_proxy_load` (у метрики есть labels `ip` и `port`, Grafana покажет по одной линии на каждый прокси).
- Visualization: **Time series**.
- Title: `Нагрузка по free-прокси (по ip:port)`.

При желании в **Transform** можно добавить «Labels to fields», чтобы видеть ip/port в таблице.

---

### 5.3. Краткая шпаргалка по интерфейсу

| Действие | Где |
|----------|-----|
| Выбрать источник данных | Вверху панели: выпадающий список (Prometheus) |
| Ввести запрос | Внизу: поле **Metric** или текст запроса (например `antiblock_users_total`) |
| Сменить тип графика | Справа: **Visualization** → Stat / Time series / Gauge / Table |
| Заголовок панели | Справа: **Panel options** → **Title** |
| Сохранить панель | **Apply** |
| Сохранить дашборд | Вверху справа: **Save dashboard** (иконка дискеты) |

### 5.4. Готовый дашборд из JSON (опционально)

Можно один раз создать дашборд, экспортировать его в JSON и потом импортировать на другом инстансе: **☰ → Dashboards → New → Import** → вставить JSON или загрузить файл. Либо положить JSON в `monitoring/grafana/provisioning/dashboards/` и настроить provisioning — тогда дашборд появится при старте Grafana автоматически (см. [Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards)).

---

## 6. Остановка и данные

- Остановка стека мониторинга:
  ```bash
  docker compose -f docker-compose.monitoring.yml down
  ```
- Данные Prometheus и Grafana хранятся в Docker-томах `prometheus_data` и `grafana_data`. При `down` без `-v` они сохраняются.
- Срок хранения данных Prometheus: 30 дней (`--storage.tsdb.retention.time=30d` в `docker-compose.monitoring.yml`).

---

## Сводка по домену notecalendar.ru

| Сервис   | Внутренний адрес      | Внешний URL (после настройки nginx)   |
|----------|------------------------|----------------------------------------|
| Метрики бота | http://bot:9090/metrics | только внутри Docker-сети             |
| Prometheus   | http://127.0.0.1:9091  | опционально https://notecalendar.ru/prometheus/ |
| Grafana      | http://127.0.0.1:3000  | **https://notecalendar.ru/grafana/**  |

Конфигурация Grafana для notecalendar.ru задана в `docker-compose.monitoring.yml` переменными `GF_SERVER_ROOT_URL` и `GF_SERVER_SERVE_FROM_SUB_PATH`.
