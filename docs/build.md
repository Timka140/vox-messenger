# Сборка и релизы Vox

Все скрипты в `build/`. Запускать из корня репо.

## Что где запускается

| Сценарий | Где |
|---|---|
| Локальная Windows-сборка для разработки | Windows cmd |
| Linux-сборка signal-server, deploy на VPS | WSL |
| Релиз клиента (`release.sh`) | WSL (нужен mingw + linux toolchain) |

---

## 1. Локальная разработка

### Windows (cmd)
```cmd
build\app-windows.bat
```
Результат: `build\out\nexus.exe`. Версия = `dev`, автообновление отключено.

### Linux (WSL)
```bash
./build/app-linux.sh
```
Требует: `libgtk-3-dev`, `libwebkit2gtk-4.1-dev`, `libopus-dev`.
**Внимание:** `third_party/opus/libopus.a` собрана под mingw (Windows). Для Linux нужна system libopus или отдельная статик-либа (см. «Известные проблемы»).

---

## 2. Узлы кластера и деплой серверов

Серверы Vox (signal, SFU, debug, coturn) крутятся на нескольких узлах.
Контракт кластера — `docs/cluster-contract.md`.

| Узел | Конфиг | Каталог |
|---|---|---|
| vps1 | `build/nodes/vps1.conf` | `~/vox` |
| vps2 | `build/nodes/vps2.conf` | `/home/vox/vox` |

Конфиг узла (`build/nodes/<node>.conf`, в git) — адреса и pubkey сигналки.
Секреты узла — `build/remote/<node>/` (`.env`, `sfu.env`, `vox-signal.key`,
`ssh.conf` с логином и портом SSH, пример — `build/nodes/ssh.conf.example`),
в `.gitignore`. Локальный `vox-signal.key` — source of truth: потеря = смена
ключа узла.

```bash
./build/signal-linux.sh && ./build/sfu-linux.sh && ./build/debug-linux.sh

./build/deploy-signal.sh              # vps1 (по умолчанию)
NODE=vps2 ./build/deploy-signal.sh    # любой узел из build/nodes/
NODE=vps2 ./build/deploy-sfu.sh
NODE=vps2 ./build/deploy-debug.sh     # после deploy-signal (читает его .env)

DRY_RUN=1 NODE=vps2 ./build/deploy-signal.sh   # только напечатать команды
```

`deploy-signal.sh` делает бэкап `signal-server.bak-YYYYMMDD-HHMMSS` перед
заменой и проверяет `/health`. Только для узла с `NODE_PINNED=1` (vps1) pubkey
синхронизируется в клиентский `known_servers.go`.

Кластер: `deploy-signal.sh` генерирует `build/remote/<node>/cluster.env` из
всех `build/nodes/*.conf` (`CLUSTER_SELF_ADDR`, `CLUSTER_PEERS`,
`CLUSTER_SOFT_LIMIT`) и заливает рядом с `.env`; `start-signal.sh` его
подхватывает. Новый узел попадает в кластер после деплоя сигналки на все
узлы. Состояние линков: `curl http://<ip>:9741/healthz`.

Тестовый клиент на конкретном узле (только `ENV=local`):
```powershell
$env:ENV="local"; $env:VOX_SIGNAL_SERVERS="n2.northwindcdn.com:9741=<pubkey vps2>"
```

### Новый узел

1. `build/nodes/<node>.conf` по образцу vps2 (`NODE_SETUP=auto`, `NODE_ROOT_SSH`, relay-порты TURN).
2. `build/remote/<node>/.env` и `sfu.env` по образцу vps2: свой `TURN_URL`,
   `SFU_ENDPOINT`, `RELEASES_DIR`, свой `SFU_SECRET` (одинаковый в обоих файлах),
   общие `TURN_USERNAME`/`TURN_CREDENTIAL`.
3. `./build/setup-node.sh <node>` — пользователь, coturn (relay только в
   публичные сети), правила ufw. Существующие правила ufw не трогает.
4. Деплой трёх сервисов с `NODE=<node>`; `NODE_PUBKEY` из вывода вписать в конфиг.
5. `./build/sync-releases.sh <node>` — скопировать релизы с vps1.
6. SSH из WSL: `ssh-keyscan -t ed25519 <ip> >> ~/.ssh/known_hosts` (сверив
   отпечаток), иначе `release.sh` из WSL молча повиснет.

vps1 настроен вручную (`build/setup-server.sh` под sudo), `setup-node.sh` для него не запускается.

Проверка после деплоя:
```bash
curl http://n1.northwindcdn.com:9741/health
curl http://n2.northwindcdn.com:9741/api/update/manifest
(cd server/signal && go run ./cmd/grouptest --addr=ws://n2.northwindcdn.com:9741/ws)
```
**Важно:** update endpoints висят на **WS-порту 9741**, не на 9750 (9750 — это debug logs API).

Собрать логи с VPS:
```bash
./build/fetch-logs.sh             # → .local/logs/
```

---

## 3. Релиз клиента с автообновлением

### Однократная подготовка

1. Сгенерировать Ed25519 keypair (из WSL):
   ```bash
   cd app && go run ./cmd/sign-release -gen-key ../.local/release-sign
   mv ../.local/release-sign.pub ../build/
   ```
   Приватный `.local/release-sign.key` в git не попадает (вся `.local/` в `.gitignore`), публичный `build/release-sign.pub` коммитится.

2. Установить mingw для Windows-кросс-сборки:
   ```bash
   sudo apt install mingw-w64
   ```

### Выпуск релиза

```bash
./build/release.sh v0.00.000.0001 "Changelog"
# или с force-update модалкой:
./build/release.sh v0.00.000.0002 "Critical fix" force
```

Формат версии строгий: `v<major>.<minor:2>.<patch:3>.<build:4>` (regex `^v\d+\.\d{2}\.\d{3}\.\d{4}$`).

Что делает скрипт:
1. Билдит `sign-release` и `manifest-merge` утилиты
2. Кросс-компилит Windows (mingw) и Linux
3. Подписывает каждый бинарь Ed25519
4. Подтягивает существующий `manifest.json` с VPS, мерджит новый релиз
5. Заливает артефакты + manifest в `~/vox/releases/` на VPS

Линукс-сборка сейчас падает (см. ниже) — release.sh skip-нет её и продолжит только с Windows. Это нормально для текущих тестеров.

### Локальный билд клиента с конкретной версией (без релиза)

Нужен, чтобы проверить, что dev-клиент видит обновление с сервера. Версия должна быть **меньше** залитой на сервер.

Из Windows (cmd):
```cmd
cd D:\projects\src\projects\Messanger\app
set /p PUBKEY=<..\build\release-sign.pub
go build -ldflags="-H windowsgui -X main.AppVersion=v0.00.000.0000 -X main.UpdatePublicKey=%PUBKEY%" -o ..\build\out\nexus.exe .
```

Из WSL:
```bash
PUBKEY=$(cat build/release-sign.pub | tr -d '\n\r ')
cd app
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -ldflags="-H windowsgui -X main.AppVersion=v0.00.000.0000 -X main.UpdatePublicKey=$PUBKEY" \
  -o ../build/out/nexus.exe .
```

Без `-X main.AppVersion=...` версия будет `dev` и автообновление не активируется (`update: dev build, auto-update disabled`).

---

## 4. Как работает автообновление (кратко)

- При `auth_ok` от signal-сервера в payload приходит `LatestRelease{Version, Force, Changelog}`.
- Клиент сравнивает с `AppVersion` (ldflags). Если новее — показывает уведомление в Settings → Обновления.
- Скачивание: `GET /api/update/download/<platform>/<version>/<filename>` → SHA-256 + Ed25519 verify → `.vox-update/<ver>/nexus.exe`.
- Apply: rename trick (Windows: `update_apply_windows.go`, Unix: `update_apply_unix.go`). Старый бинарь сохраняется как `nexus.exe.old-<prev>`.
- Rollback: Settings → Обновления → «Откатиться на v...» (single-slot, только последний backup).
- Force update: если `force=true` в манифесте — модалка блокирует UI до обновления.

Подробнее — в memory `project_auto_update.md`.

---

## Известные проблемы

### Linux-сборка клиента падает с `___chkstk_ms`
Причина: `third_party/opus/libopus.a` — это mingw-сборка для Windows. Linux линкер не понимает mingw runtime символы.

**Workaround сейчас:** релизить только Windows. `release.sh` сам skip-нет упавшую платформу.

**Чтобы починить полностью:** либо вынести Linux libopus в отдельный path и использовать build tags, либо линковать с system libopus (`sudo apt install libopus-dev` + поправить cgo LDFLAGS в `app/internal/codec/opus.go`).

### MinGW не установлен
```
cgo: C compiler "x86_64-w64-mingw32-gcc" not found
```
Фикс: `sudo apt install mingw-w64`.

### `404 page not found` на `/api/update/manifest`
Вы запросили не тот порт. Update endpoints на **9741** (WS-порт), не на 9750.

### Клиент пишет «нет обновлений», хотя релиз залит
Проверьте лог клиента: если там `update: dev build, auto-update disabled` — значит бинарь собран без `-X main.AppVersion=...`. Нужно пересобрать с ldflags.

---

## Артефакты на VPS

```
~/nexus/
├── signal-server               # бинарь сервера
├── .env                        # конфиг
├── start-signal.sh / stop-signal.sh
├── debug-logs/                 # логи от клиентов
└── releases/
    ├── manifest.json
    ├── windows-amd64/
    │   └── v0.00.000.0001/
    │       ├── nexus.exe
    │       └── nexus.exe.sig
    └── linux-amd64/
        └── ...
```

SSH-доступ к узлу — `build/remote/<node>/ssh.conf`.

## Порты

| Порт | Назначение |
|---|---|
| 9741 | Signal WS + `/api/update/*` + `/health` |
| 9742 | Debug HTTP (localhost only) |
| 9750 | Debug logs API (`/api/sources` etc.) |
| 3478 | TURN (coturn) |
