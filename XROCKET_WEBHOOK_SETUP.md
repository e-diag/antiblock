# Настройка webhook xRocket Pay (домен notecalendar.ru)

## 1. DNS

В панели управления доменом создайте A-запись:

| Тип | Имя | Значение | TTL |
|-----|-----|----------|-----|
| A   | webhook | 109.120.139.214 | 300 |

Итог: `webhook.notecalendar.ru` → ваш сервер. Подождите 5–15 минут, пока DNS обновится.

---

## 2. Nginx + Let's Encrypt

На сервере (Ubuntu/Debian):

```bash
sudo apt update
sudo apt install -y nginx certbot python3-certbot-nginx
```

---

## 3. Конфиг Nginx

Создайте файл `/etc/nginx/sites-available/xrocket-webhook`:

```nginx
server {
    listen 80;
    server_name webhook.notecalendar.ru;
    location /webhook/xrocket {
        proxy_pass http://127.0.0.1:8081;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Включите сайт и проверьте конфиг:

```bash
sudo ln -sf /etc/nginx/sites-available/xrocket-webhook /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

---

## 4. SSL (Let's Encrypt)

```bash
sudo certbot --nginx -d webhook.notecalendar.ru
```

Следуйте подсказкам (email, согласие). Certbot сам настроит HTTPS в nginx.

---

## 5. URL для xRocket

В боте @xrocket → Rocket Pay → ваше приложение → Webhooks укажите:

```
https://webhook.notecalendar.ru/webhook/xrocket
```

Сохраните.

---

## 6. Проверка

```bash
curl -X POST https://webhook.notecalendar.ru/webhook/xrocket -d '{}' -H "Content-Type: application/json"
```

Должен вернуться `400` или `403` (не `404` и не `Connection refused`) — значит, endpoint доступен.

---

## 7. Порт 8081

В `docker-compose.yml` порт уже проброшен:

```yaml
ports:
  - "127.0.0.1:8081:8081"
```

Бот слушает webhook на `http://127.0.0.1:8081`. Nginx принимает HTTPS снаружи и проксирует на этот порт.
