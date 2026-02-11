## CI/CD с GitHub Actions для AntiBlock

В этом файле описаны шаги по настройке GitHub Actions для автоматической проверки и деплоя проекта.

### 1. Подготовка репозитория

1. **Инициализируйте git-репозиторий (если ещё нет)**:

   ```bash
   git init
   git add .
   git commit -m "Initial commit"
   ```

2. **Создайте репозиторий на GitHub** и привяжите локальный:

   ```bash
   git remote add origin git@github.com:USERNAME/AntiBlock.git
   git branch -M main
   git push -u origin main
   ```

   Замените `USERNAME` на ваш GitHub-логин и при необходимости имя репозитория.

### 2. Что делает workflow `.github/workflows/ci-cd.yml`

- **Триггер**:
  - Запускается на `push` и `pull_request` в ветки `main` и `master`.
- **jobs.test**:
  - Ставит Go \(версия берётся из `GO_VERSION`, сейчас `"1.23"`\).
  - Кэширует модули.
  - Запускает:
    - `go test ./...`
    - `go vet ./...`
    - `go build ./cmd/bot` (просто проверка сборки).
- **jobs.docker-build**:
  - После успешных тестов собирает Docker-образ по `Dockerfile` (без пуша в реестр) — проверка, что образ собирается.
- **jobs.deploy**:
  - Запускается **только** на `main`/`master` и только после успешных `test` и `docker-build`.
  - Подключается к прод-серверу по SSH и выполняет:

    ```bash
    cd "${APP_DIR}"
    git pull
    docker compose pull || docker-compose pull
    docker compose up -d --build || docker-compose up -d --build
    ```

  - Ожидается, что на сервере уже установлен Docker и (docker) compose, а репозиторий **уже клонирован** в `APP_DIR`.

### 3. Настройка секретов в GitHub

1. Зайдите в репозиторий на GitHub.
2. Откройте: **Settings → Secrets and variables → Actions → New repository secret**.
3. Создайте следующие секреты (названия должны совпадать с теми, что используются в workflow):

- **`SSH_HOST`**: IP или доменное имя сервера, куда деплоим (например, `123.45.67.89`).
- **`SSH_USER`**: пользователь на сервере (например, `ubuntu` или `root`).
- **`SSH_PORT`**: порт SSH (обычно `22`).
- **`SSH_KEY`**: приватный SSH-ключ (формат `-----BEGIN OPENSSH PRIVATE KEY----- ...`), который имеет доступ к серверу.
  - Лучше создать отдельную пару ключей специально под деплой.
  - Публичный ключ добавьте на сервер в `~/.ssh/authorized_keys` соответствующего пользователя.
- **`APP_DIR`**: путь на сервере, где лежит ваш проект (например, `/var/www/antiblock`).

### 4. Подготовка сервера для деплоя

1. **Установка Docker и docker compose** (пример для Ubuntu):

   ```bash
   sudo apt update
   sudo apt install -y docker.io
   sudo usermod -aG docker $USER
   # Перелогиньтесь, чтобы группа применилась

   # docker compose v2 (если ещё нет)
   sudo curl -L "https://github.com/docker/compose/releases/download/v2.29.0/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose
   sudo chmod +x /usr/local/bin/docker-compose
   ```

2. **Клонируйте репозиторий на сервере** в директорию, совпадающую с `APP_DIR`:

   ```bash
   mkdir -p /var/www
   cd /var/www
   git clone git@github.com:USERNAME/AntiBlock.git antiblock
   cd antiblock
   ```

   В этом случае:

   - `APP_DIR` в секретах GitHub должен быть `/var/www/antiblock`.

3. **Настройте `.env` или переменные окружения**:

   - На сервере создайте `.env` или экспортируйте переменные окружения (например, для `docker-compose.yml`).
   - Можно создать `.env` рядом с `docker-compose.yml`, используя `.env.example` как шаблон.

### 5. Как работает деплой

1. Вы пушите изменения в ветку `main` (или создаёте PR → мёрджите в `main`).
2. GitHub Actions запускает workflow `CI/CD`:
   - Сначала **тесты и линтеры**.
   - Затем **сборка Docker-образа**.
   - Если всё прошло успешно, запускается job **deploy**.
3. Job **deploy** по SSH заходит на сервер:
   - `cd "${APP_DIR}"` — в директорию с репозиторием.
   - `git pull` — подтягивает последнюю версию кода.
   - `docker compose up -d --build` — перестраивает и перезапускает контейнеры (или `docker-compose`, если v1).

### 6. Частые места для настройки под себя

- **Ветки**:
  - В секции `on.push.branches` и `on.pull_request.branches` можно оставить только ту ветку, с которой вы реально работаете (например, только `main`).
- **Версия Go**:
  - В `env.GO_VERSION` укажите ту версию, под которую реально разрабатываете.
- **SSH и путь на сервере**:
  - Убедитесь, что:
    - `SSH_HOST`, `SSH_USER`, `SSH_PORT`, `SSH_KEY`, `APP_DIR` корректно заполнены.
    - Ключ из `SSH_KEY` действительно имеет доступ на сервер.

### 7. Проверка, что всё настроено верно

1. Сделайте небольшой коммит (например, изменение в `README.md`).
2. Запушьте в `main`:

   ```bash
   git add README.md
   git commit -m "Test CI/CD"
   git push
   ```

3. Зайдите во вкладку **Actions** в репозитории на GitHub и посмотрите, как выполняется workflow:
   - Убедитесь, что `test` и `docker-build` проходят.
   - Убедитесь, что `deploy` отработал без ошибок.
4. Зайдите на сервер и проверьте:
   - Логи контейнеров: `docker compose logs -f` или `docker-compose logs -f`.
   - Что приложение запущено и работает, как ожидается.

