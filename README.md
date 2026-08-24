# Telegram Anti-Spam Bot

Антиспам-бот для Telegram-групп на Go. Группа работает в режиме заявок на вступление:

- по заявке (`chat_join_request`) бот НЕ одобряет вход, заявка остается в очереди Telegram;
- владелец получает в личку карточку с профилем заявителя и кнопками;
- решение владельца: `Разрешить` / `Отклонить` / `Бан`, после бана — `Разбанить`.

Пока заявка не одобрена, пользователь не в группе: он не может ни писать, ни читать чат.
Ограничение прав участника (`restrictChatMember`) не используется — блокировка обеспечивается
самой очередью заявок, поэтому нет промежутка, в котором принятый спамер успевает написать.

## Требования

- Go 1.22+
- публичный HTTPS (TLS терминирует nginx)
- в группе включен режим `Заявки на вступление` (Тип группы -> Кто может вступать)
- бот в группе должен быть админом с правами:
  - приглашать пользователей / управлять заявками (`can_invite_users`) — обязательно,
    без него не приходит `chat_join_request` и не работает одобрение
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
- `PENDING_TTL` — через сколько нерассмотренная заявка отклоняется автоматически
  (формат Go: `48h`, `30m`). Пусто или `0` — авто-отклонение выключено, заявка ждет решения.

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

1. Бот админ в тестовой группе, у группы включен режим заявок.
2. Новый пользователь подает заявку на вступление.
3. Владелец получает в личку карточку: имя, @username, ID, bio, время заявки, токен.
   Кнопки: `Разрешить`, `Отклонить`, `Бан`.
4. `Разрешить` — пользователь попадает в группу, карточка становится зеленой.
5. `Отклонить` — заявка снята, пользователь может подать ее повторно;
   на карточке остается кнопка `Бан`, которая закрывает повторные заявки.
6. `Бан` — карточка красная, появляется кнопка `Разбанить`.
7. Если владелец принял или забанил заявителя в родном интерфейсе Telegram,
   бот увидит это по `chat_member` и сам закроет свою карточку.

## Обновление с версии с вопросом в личке

Прежний контур (ограничение прав при входе, deep-link, вопрос в личке) удален вместе
с переменными `BOT_QUESTION` и `GROUP_PROMPT_TEXT`. Что нужно учесть при переезде:

- включите в группе режим заявок и выдайте боту право `can_invite_users`;
- таблица `pending_verifications` остается в БД нетронутой, новый контур пишет в `join_requests`;
- пользователи, которых старая версия успела ограничить в правах, останутся ограниченными:
  снимите ограничение вручную в интерфейсе Telegram — новый код `restrictChatMember` не вызывает.

## Частые проблемы

- Ошибка `parse OWNER_ID ... invalid syntax` с `\r`:
  - env-файл в CRLF. Приведите к LF (`dos2unix /etc/tg-block.env`).
  - В коде уже есть `TrimSpace`, но лучше держать env в Unix-формате.
- Нет реакции на заявку:
  - в группе не включен режим `Заявки на вступление` — тогда `chat_join_request` не приходит;
  - у бота нет права `can_invite_users`;
  - webhook не установлен/недоступен либо неверный путь webhook;
  - webhook зарегистрирован старой версией без `chat_join_request` в `allowed_updates` —
    перезапустите бот с заданным `WEBHOOK_BASE_URL`, он переустановит webhook.
- Владелец не получает карточки:
  - проверьте корректность `OWNER_ID` (должен быть числовой id).
- В логах `telegram call failed, retrying` с `rate_limited=true`:
  - это штатная реакция на 429, клиент ждет `retry_after` и повторяет вызов;
  - если сообщений много, посмотрите на всплеск заявок: отправка владельцу
    ограничена примерно одним сообщением в секунду.
- Пользователь не получил уведомление об одобрении:
  - ожидаемо, если он не начинал диалог с ботом; на решение это не влияет.

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