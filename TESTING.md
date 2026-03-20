# Чек-лист локального тестирования

## Подготовка

- [ ] Тестовый бот @BotFather → токен в `.env.test`
- [ ] `createdb antiblock_test` (или первый запуск подскажет)
- [ ] `go build ./...` — без ошибок
- [ ] Первый запуск `go run ./cmd/bot` с `DB_NAME=antiblock_test` (миграции)
- [ ] `psql -d antiblock_test -f scripts/setup_test_db.sql`

При **`go run ./cmd/bot`** из корня репозитория автоматически читаются файлы **`.env`**, затем **`.env.test`** (перекрывает совпадающие ключи). Docker для самого бота не нужен; нужен только **PostgreSQL** (локально или в Docker).

Альтернатива без файлов (PowerShell):  
`$env:TELEGRAM_BOT_TOKEN="..."; $env:TELEGRAM_ADMIN_ID_1="123456789"; go run ./cmd/bot`

## Блок А — Старт и миграции

- [ ] Бот стартует без паники
- [ ] Лог: `Pro Docker manager initialized` или предупреждение о сертификатах
- [ ] Таблицы: `pro_groups`, `pro_subscriptions`, `premium_servers`, `vps_provision_requests`
- [ ] Колонки у `proxy_nodes`: `secret_ee`, `floating_ip`, `timeweb_floating_ip_id`
- [ ] Индекс `idx_user_proxies_unique`

## Блок Б — Free прокси

- [ ] `/start` → меню (Free, Pro, Premium)
- [ ] Получить прокси → выдача
- [ ] Повторный запрос → без дублей в «Мои прокси»

## Блок В — Pro

- [ ] Pro → экран с ценой
- [ ] Выдача Pro через БД → два сообщения (dd + ee)
- [ ] Плановая ротация групп

## Блок Г — Legacy Premium

### Активная подписка

- [ ] `tg_id=100000002` — без лишних уведомлений при старте
- [ ] `/premium_info 100000002` → Legacy Premium, статус контейнера dd

### Истечение

- [ ] `UPDATE users SET premium_until = NOW() - interval '1 minute' WHERE tg_id=100000002`
- [ ] Лог: `[Premium] legacy containers removed for tg_id=...`
- [ ] `proxy_nodes.status = inactive`

### Продление → новый Premium (TimeWeb)

- [ ] `/grantpremium 100000002 30`
- [ ] Лог: `legacy upgrade` → `ProvisionForUser` (при токене) или `provisioner not configured`
- [ ] При пустом `TIMEWEB_API_TOKEN` — одно сообщение «⏳ Ваш персональный прокси...»

## Блок Д — Новый Premium без TimeWeb

- [ ] Пустой `TIMEWEB_API_TOKEN` — бот не паникует
- [ ] `/grantpremium <новый_tg_id> 30` — сообщение об ожидании / очередь

## Блок Е — Панель менеджера

- [ ] Пагинация прокси: Free / Pro / Premium / Все
- [ ] Статистика Legacy / New Premium (если есть в панели)
- [ ] Двойной `mgr_vps_confirm` — один VPS в логах

### Создание Premium VPS (очередь админа)

- [ ] Сразу после появления IPv4 в логах: `premium_servers ... в пуле до SSH/Docker` — пользователи не должны видеть «нет активного сервера», пока идёт `WaitSSH` / `SetupDocker`.
- [ ] При ошибке SSH/Docker заявка откатывается в `pending`, `timeweb_server_id` остаётся — повторное подтверждение продолжает с того же VPS в TimeWeb.

**Если VPS уже создан в TimeWeb, а в БД строки нет** (старый бот / сбой): вручную `UPDATE vps_provision_requests SET status = 'pending', timeweb_server_id = <id из TW> WHERE id = ...` и снова подтвердить в боте, либо `INSERT` в `premium_servers` с тем же `timeweb_id` и основным IPv4.

- [ ] Если у нового VPS в TimeWeb только публичный IPv6, бот вызывает `POST .../servers/{id}/ips` с `type=ipv4` (платный публичный IPv4 по тарифам Timeweb), затем ждёт появления IPv4 в API и только его использует для SSH.
- [ ] Если в заявке сохранён `timeweb_server_id`, а сервер в Timeweb удалён (404), при подтверждении бот сбрасывает id на заявке, удаляет устаревшую строку `premium_servers` и создаёт **новый** VPS.

## Блок Ж — TimeWeb (второй этап)

- [ ] Заполнить `TIMEWEB_API_TOKEN`
- [ ] `/grantpremium <tg_id> 1` — у каждого пользователя **свой** floating IP (в т.ч. первый); в панели TimeWeb виден FIP; в `tg://proxy` только FIP, не основной IP VPS
- [ ] В логах SSH после Docker: `docker pull nineseconds/mtg:2` и `docker pull p3terx/mtg` ([ee](https://hub.docker.com/r/nineseconds/mtg/), [dd](https://hub.docker.com/r/p3terx/mtg))
- [ ] Telegram: 443 (ee), 8443 (dd)
- [ ] `/replace_ip` — новый IP, уведомления
- [ ] Продление — перезапуск контейнеров, те же секреты
- [ ] Истечение — освобождение floating IP

## Команды

```bash
go build ./...
go vet ./...
```

Очистка тестовой БД: `psql -d antiblock_test -f scripts/cleanup_test_db.sql`
