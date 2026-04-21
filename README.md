# Telegram Anti-Spam Bot

Антиспам-бот для Telegram-групп на Go:

- при входе пользователя в группу бот ограничивает отправку сообщений;
- отправляет ссылку на личный старт с ботом;
- в личке задает статический вопрос;
- пересылает ответ владельцу;
- владелец принимает решение: допустить / кик+бан / разбанить.

## Требования

- Go 1.22+
- публичный HTTPS (TLS терминирует nginx)
- бот в группе должен быть админом с правами:
  - ограничивать участников
  - банить участников

## Переменные окружения

Обязательные:

- `BOT_TOKEN`
- `OWNER_ID`
- `WEBHOOK_SECRET`

Основные:

- `WEBHOOK_PATH` (по умолчанию `/webhook`)
- `WEBHOOK_BASE_URL` (например `https://example.com`)
- `WEBHOOK_PUBLIC_URL` (опционально; если задан, имеет приоритет)
- `LISTEN_ADDR` (по умолчанию `:8080`)
- `DB_PATH` (по умолчанию `bot.db`)
- `BOT_QUESTION`
- `GROUP_PROMPT_TEXT`

Пример — в `.env.example`.

## Сборка

### Локально (Windows)

```powershell
Set-Location c:\SG\IPYNB\tg_block
go build -o tg-anti-spam.exe ./cmd/bot
```

### Кросс-компиляция Linux

```powershell
Set-Location c:\SG\IPYNB\tg_block
powershell -ExecutionPolicy Bypass -File .\scripts\build-linux.ps1 -Arch amd64
```

или:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build-linux.ps1 -Arch arm64
```

Бинарники: `dist/tg-anti-spam-linux-amd64`, `dist/tg-anti-spam-linux-arm64`.
Архивы: `dist/tg-anti-spam-linux-amd64.tar.gz`, `dist/tg-anti-spam-linux-arm64.tar.gz`.

## Автопубликация релизов в GitHub

В репозитории настроен workflow `.github/workflows/release.yml`.

Что происходит:

- при пуше тега формата `v*` (например `v1.2.0`) запускается GitHub Actions;
- собираются Linux-бинарники `amd64` и `arm64`;
- бинарники упаковываются в `.tar.gz`;
- публикуется GitHub Release с архивами и файлом `SHA256SUMS`.

Как выпустить релиз:

```powershell
git tag v1.2.0
git push origin v1.2.0
```

Ручной запуск:

- в GitHub откройте `Actions` -> `Release` -> `Run workflow`;
- укажите `tag` в формате `v*` (например `v1.2.0`).

## Запуск на Alpine Linux

Бот читает конфиг из env-переменных. `.env` автоматически не подхватывается.

Рекомендованный путь:

1. создать отдельного пользователя, например `tgblock`;
2. положить бинарник в `/usr/local/bin/tg-anti-spam`;
3. создать каталог под БД, например `/var/lib/tg-block`;
4. хранить env в `/etc/tg-block.env` (права `600`).

Пример запуска вручную:

```sh
set -a
. /etc/tg-block.env
set +a
exec /usr/local/bin/tg-anti-spam
```

## Автозагрузка на Alpine (OpenRC)

`/etc/conf.d/tg-block`:

```sh
export BOT_TOKEN="..."
export OWNER_ID="123456789"
export WEBHOOK_SECRET="..."
export WEBHOOK_BASE_URL="https://your.domain"
export WEBHOOK_PATH="/webhook"
export LISTEN_ADDR="127.0.0.1:8080"
export DB_PATH="/var/lib/tg-block/bot.db"
```

`/etc/init.d/tg-block`:

```sh
#!/sbin/openrc-run

name="tg-block"
description="Telegram anti-spam bot"

command="/usr/local/bin/tg-anti-spam"
command_user="tgblock"
command_background="yes"
pidfile="/run/${RC_SVCNAME}.pid"

depend() {
    need net
    after firewall
}
```

Команды:

```sh
chmod +x /etc/init.d/tg-block
rc-update add tg-block default
rc-service tg-block start
rc-service tg-block status
```

## Nginx

Пример конфига: `deploy/nginx.conf.example`.

Важно:

- проксировать путь `/webhook/` на `LISTEN_ADDR`;
- TLS должен быть валиден снаружи;
- `WEBHOOK_BASE_URL`/`WEBHOOK_PUBLIC_URL` должны соответствовать реальному домену.

## Проверка работы

### 1) Проверка webhook у Telegram

Откройте:

`https://api.telegram.org/bot<BOT_TOKEN>/getWebhookInfo`

Проверьте:

- `url` корректный;
- `last_error_message` пустой.

### 2) Проверка доступности endpoint

```sh
curl -X POST "https://ваш-домен/webhook/<WEBHOOK_SECRET>" \
  -H "Content-Type: application/json" \
  -d "{}"
```

### 3) E2E сценарий

1. Бот админ в тестовой группе.
2. Новый пользователь вступает в группу.
3. Бот ограничивает пользователя и отправляет deep-link.
4. Пользователь в личке нажимает Start и отвечает на вопрос.
5. Владелец получает пересланный ответ и кнопки:
  - `Пустить`
  - `Кик+бан`
6. После `Кик+бан` появляется кнопка `Разбанить`.
7. После `Пустить`/`Разбанить` статус в сообщении владельцу становится зеленым.

## Частые проблемы

- Ошибка `parse OWNER_ID ... invalid syntax` с `\r`:
  - env-файл в CRLF. Приведите к LF (`dos2unix /etc/tg-block.env`).
  - В коде уже есть `TrimSpace`, но лучше держать env в Unix-формате.
- Нет реакции на вход:
  - у бота нет админ-прав `restrict/ban`;
  - webhook не установлен/недоступен;
  - неверный путь webhook.
- Владелец не получает действия:
  - проверьте корректность `OWNER_ID` (должен быть числовой id).

## Production hardening

### 1) Ограничить доступ к webhook по IP Telegram

Дополните `location /webhook/` в nginx:

```nginx
# Список адресов Telegram периодически меняется.
# Актуальные диапазоны проверяйте в документации Telegram.
allow 149.154.160.0/20;
allow 91.108.4.0/22;
deny all;
```

Примечания:

- это дополнительная защита поверх `WEBHOOK_SECRET`;
- после изменения диапазонов не забудьте `nginx -t && nginx -s reload`.

### 2) Резервное копирование SQLite

Если `DB_PATH=/var/lib/tg-block/bot.db`, минимальный вариант:

```sh
mkdir -p /var/backups/tg-block
cp /var/lib/tg-block/bot.db /var/backups/tg-block/bot-$(date +%F-%H%M%S).db
find /var/backups/tg-block -type f -name "bot-*.db" -mtime +14 -delete
```

Рекомендации:

- запускать по cron 1-4 раза в сутки;
- хранить копии минимум на другом диске/хосте;
- периодически проверять восстановление (тестовый запуск с backup-файлом).

### 3) Ротация логов

Если сервис пишет в файл (через перенаправление OpenRC), настройте `logrotate`.

Пример `/etc/logrotate.d/tg-block`:

```conf
/var/log/tg-block/*.log {
  daily
  rotate 14
  compress
  delaycompress
  missingok
  notifempty
  copytruncate
}
```

Если логи идут в системный логгер (`logread`), проверьте политики ротации syslog на хосте.