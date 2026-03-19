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

## Блок Ж — TimeWeb (второй этап)

- [ ] Заполнить `TIMEWEB_API_TOKEN`
- [ ] `/grantpremium <tg_id> 1` — floating IP в панели TimeWeb
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
