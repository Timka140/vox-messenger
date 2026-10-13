# Сборка и релизы Vox

Все скрипты в `build/`. Запускать из корня репо.

## Что где запускается

| Сценарий | Где |
|---|---|
| Локальная Windows-сборка для разработки | Windows cmd (MinGW в PATH) |
| Linux-сборки серверов, deploy на узлы | WSL или Git Bash |
| Релиз клиента (`release.sh`) | WSL (mingw-w64 + нативный Linux toolchain) |

---

## 0. Подготовка исходников

RNNoise подключён git-сабмодулем, веса модели в upstream не входят:

```bash
git submodule update --init app/internal/audio/rnnoise
(cd app/internal/audio/rnnoise && ./download_model.sh)   # нужен wget; создаёт src/rnnoise_data.c
```

Для тега `nx_onnx` (Silero VAD, FastEnhancer, GTCRN) — onnxruntime:

```bash
(cd app/internal/ml/onnx/lib && ./download_runtime.sh)  # → lib/<platform>/onnxruntime.dll | libonnxruntime.so
```

Модели, которые едут рядом с exe (в git не лежат):

```
.local/models/DeepFilterNet3.tar.gz
.local/models/VAD/silero_vad.onnx
```

Для тега `nx_dfn` (DeepFilterNet3) нужен Rust: `third_party/libdf` собирается
`cargo build --release` (для Windows из WSL — `--target x86_64-pc-windows-gnu`,
с `RUST_MIN_STACK=33554432`). `release.sh` делает это сам, если есть `cargo`.

### Теги сборки

| Тег | Что включает | Требует |
|---|---|---|
| `nx_rnnoise` | шумодав RNNoise | сабмодуль + `download_model.sh` |
| `nx_onnx` | Silero VAD, FastEnhancer, GTCRN | onnxruntime рядом с exe, `models/VAD/` |
| `nx_dfn` | DeepFilterNet3 | собранный `third_party/libdf`, `models/DeepFilterNet3.tar.gz` |

Без тегов клиент собирается и работает, но без шумодава и VAD.

---

## 1. Локальная разработка

Фронтенд — обычный JS без сборщика, встраивается в бинарь через `go:embed`;
после правок JS/CSS достаточно пересобрать Go.

### Windows (cmd)

```cmd
build\app-windows.bat                    :: dev: build\out\dev\vox.exe, без тегов
build\app-windows.bat v0.00.000.0000     :: с версией: build\out\release\, теги + onnxruntime.dll + модели
```

Dev-сборка: версия `dev`, автообновление отключено. Сборка с версией берёт
теги `nx_rnnoise nx_onnx` (и `nx_dfn`, если собран libdf) и кладёт рядом
onnxruntime и модели — это готовая папка для ручной раздачи.

Нужны gcc и g++ (MinGW-w64): openh264 собирается через C++-мост.

### Linux

```bash
./build/app-linux.sh                     # → build/out/vox, без тегов
```

Требует: gcc, g++, `libgtk-3-dev`, `libwebkit2gtk-4.1-dev`. libopus — статическая
из `third_party/opus/linux-amd64/`, системная не нужна. Запуск требует glibc 2.38+.

### Тестовый клиент на конкретном узле (только `ENV=local`)

```powershell
$env:ENV="local"; $env:VOX_SIGNAL_SERVERS="n2.northwindcdn.com:9741=<pubkey vps2>"
```

---

## 2. Узлы кластера и деплой серверов

Серверы Vox (signal, SFU, debug, coturn) крутятся на нескольких узлах.
Контракт кластера — `docs/cluster-contract.md`.

| Узел | Конфиг | Каталог |
|---|---|---|
| vps1 | `build/nodes/vps1.conf` | `~/vox` |
| vps2 | `build/nodes/vps2.conf` | `/home/vox/vox` |

Конфиг узла (`build/nodes/<node>.conf`, в git) — имя узла и pubkey сигналки.
Секреты узла — `build/remote/<node>/` (`.env`, `sfu.env`, `vox-signal.key`,
`ssh.conf` с `NODE_SSH`, `NODE_SSH_PORT`, `NODE_ROOT_SSH`, `NODE_IP`; пример —
`build/nodes/ssh.conf.example`), в `.gitignore`. Локальный `vox-signal.key` —
source of truth: потеря = смена ключа узла.

**deploy-скрипты ничего не собирают** — заливают готовый `build/out/<x>-server`.
Сначала сборка, потом деплой:

```bash
./build/signal-linux.sh && ./build/sfu-linux.sh && ./build/debug-linux.sh

./build/deploy-signal.sh              # vps1 (по умолчанию)
NODE=vps2 ./build/deploy-signal.sh    # любой узел из build/nodes/
NODE=vps2 ./build/deploy-sfu.sh
NODE=vps2 ./build/deploy-debug.sh     # после deploy-signal (читает его .env)

DRY_RUN=1 NODE=vps2 ./build/deploy-signal.sh   # только напечатать команды
```

После деплоя стоит убедиться, что на узле новый бинарь (новое поле в логе
старта или `strings signal-server | grep <новая строка>`): время файла на узле
свежее даже при заливке старой сборки.

`deploy-signal.sh` делает бэкап `signal-server.bak-YYYYMMDD-HHMMSS` (и
`groups.db`) перед заменой и проверяет `/health`. Только для узла с
`NODE_PINNED=1` (vps1) pubkey синхронизируется в клиентский `known_servers.go`.

Кластер: `deploy-signal.sh` генерирует `build/remote/<node>/cluster.env` из
всех `build/nodes/*.conf` (`CLUSTER_SELF_ADDR`, `CLUSTER_PEERS`,
`CLUSTER_SOFT_LIMIT`) и заливает рядом с `.env`; `start-signal.sh` его
подхватывает. Новый узел попадает в кластер после деплоя сигналки на все
узлы. Состояние линков: `curl http://<узел>:9741/healthz`.

### Логи клиентов на узле

Клиенты с включённой настройкой «Отправлять анонимные логи» присылают логи
под id установки `anon-<16 hex>`; сигналка хранит их в `DEBUG_DIR`
(`./debug-logs`), если задан `DEBUG_PORT`. Пределы — в `.env`:

| Переменная | По умолчанию |
|---|---|
| `DEBUG_MAX_TOTAL_MB` | 1024 |
| `DEBUG_MAX_AGE_DAYS` | 14 |
| `DEBUG_KEEP_PER_SOURCE` | 10 |

Собственный `vox-signal.log` ротируется на 50 МБ (`.1`), `signal-server.log`
(stdout, только предупреждения и ошибки) при старте уходит в `.1`.

```bash
./build/fetch-logs.sh             # → .local/logs/
```

### Новый узел

1. `build/nodes/<node>.conf` по образцу vps2 (`NODE_SETUP=auto`, relay-порты TURN);
   `build/remote/<node>/ssh.conf` с `NODE_SSH`, `NODE_ROOT_SSH`, `NODE_IP`.
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
Update endpoints висят на WS-порту **9741**.

---

## 3. Релиз клиента с автообновлением

### Однократная подготовка

1. Сгенерировать Ed25519 keypair (из WSL):
   ```bash
   cd app && go run ./cmd/sign-release -gen-key ../.local/release-sign
   mv ../.local/release-sign.pub ../build/
   ```
   Приватный `.local/release-sign.key` в git не попадает (вся `.local/` в `.gitignore`), публичный `build/release-sign.pub` коммитится.

2. Toolchain в WSL:
   ```bash
   sudo apt install mingw-w64 build-essential libgtk-3-dev libwebkit2gtk-4.1-dev
   ```
   Плюс Rust с таргетом `x86_64-pc-windows-gnu` для DeepFilterNet3 (необязательно).

3. Подготовка исходников и моделей — раздел 0.

### Выпуск релиза

Номер версии — следующий после `latest` в манифесте на узлах, а не по памяти
или git log: `manifest-merge` молча выкидывает релиз с версией ≤ latest.

```bash
ssh <узел> "jq -r '.releases[].version' ~/vox/releases/manifest.json"

./build/release.sh v0.00.000.0001 "Changelog"
# или с force-update модалкой:
./build/release.sh v0.00.000.0002 "Critical fix" force
```

Из WSL при checkout с CRLF: `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.autocrlf
GIT_CONFIG_VALUE_0=true`, иначе скрипт увидит грязное дерево и спросит подтверждение.

Формат версии строгий: `v<major>.<minor:2>.<patch:3>.<build:4>` (regex `^v\d+\.\d{2}\.\d{3}\.\d{4}$`).

Env: `RELEASE_NODES` (по умолчанию `vps1 vps2`), `KEEP_RELEASES` (3), `DRY_RUN=1`
(собрать и смерджить без заливки).

Что делает скрипт:
1. Собирает libdf (если есть `cargo`), утилиты `sign-release` и `manifest-merge`.
2. Собирает Windows (mingw, `CC`/`CXX`) и Linux (нативно) с тегами
   `nx_rnnoise nx_onnx` (+ `nx_dfn`), кладёт рядом onnxruntime и модели.
   Результат — `.local/release/<версия>/<platform>/`.
3. Забирает манифесты со всех узлов, мерджит новый релиз в самый свежий,
   оставляет `KEEP_RELEASES` версий.
4. Подписывает `manifest.json` → `manifest.json.sig` (отдельные бинари не
   подписываются: их SHA-256 записаны в подписанном манифесте).
5. Заливает на каждый узел в `$NODE_DIR/releases/` и удаляет версии, которых
   нет в манифесте (на узле нужен `jq`). Недоступный узел не валит остальные —
   скрипт напечатает команду `sync-releases.sh` для догонки.

После релиза — сайт: `design/northwind-site/publish_release.py` (вне репо).

### Локальный билд клиента с конкретной версией (без релиза)

Нужен, чтобы проверить, что клиент видит обновление с сервера. Версия должна
быть **меньше** залитой на сервер. Проще всего — `build\app-windows.bat v0.00.000.0000`.

Вручную из WSL:
```bash
PUBKEY=$(cat build/release-sign.pub | tr -d '\n\r ')
cd app
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ \
  go build -tags "nx_rnnoise nx_onnx" \
  -ldflags="-H windowsgui -X main.AppVersion=v0.00.000.0000 -X main.UpdatePublicKey=$PUBKEY" \
  -o ../build/out/vox.exe .
```

Без `-X main.AppVersion=...` версия будет `dev` и автообновление не активируется (`update: dev build, auto-update disabled`).

---

## 4. Как работает автообновление (кратко)

- При `auth_ok` от signal-сервера в payload приходит `LatestRelease{Version, Force, Changelog}`.
- Клиент скачивает `manifest.json` + `manifest.json.sig`, проверяет подпись
  ключом `UpdatePublicKey` и ставит только версию из подписанного манифеста.
- Скачивание: `GET /api/update/download/<platform>/<version>/<filename>` → сверка
  SHA-256 с манифестом → `.vox-update/<ver>/vox.exe` (+ модели, onnxruntime).
- Apply: rename trick (`update_apply_windows.go`, `update_apply_unix.go`).
  Старый бинарь сохраняется как `vox.exe.old-<prev>`.
- Rollback: Settings → О программе → «Откатиться» (single-slot, только последний backup).
- Force update: если `force=true` в манифесте — модалка блокирует UI до обновления.

---

## Известные проблемы

### MinGW не установлен
```
cgo: C compiler "x86_64-w64-mingw32-gcc" not found
```
Фикс: `sudo apt install mingw-w64`.

### `-mthreads` при кросс-сборке Windows
Не задан `CXX=x86_64-w64-mingw32-g++` — cgo берёт линуксовый g++ для openh264.

### `rnnoise_data.c: No such file` с тегом `nx_rnnoise`
Не скачаны веса RNNoise — раздел 0.

### Клиент пишет «нет обновлений», хотя релиз залит
Проверьте лог клиента: если там `update: dev build, auto-update disabled` — бинарь собран без `-X main.AppVersion=...`.

---

## Артефакты на узле

```
~/vox/                          # vps2: /home/vox/vox
├── signal-server  (+ .bak-*)   # бинарь сигналки и бэкапы
├── sfu-server, debug-server
├── .env, cluster.env, sfu.env  # конфиг
├── vox-signal.key              # ключ узла
├── groups.db (+ .bak-*)        # журнал групп
├── start-*.sh / stop-*.sh
├── vox-signal.log (+ .1)
├── debug-logs/anon-<id>/       # логи клиентов
└── releases/
    ├── manifest.json
    ├── manifest.json.sig
    ├── windows-amd64/<версия>/vox.exe, onnxruntime.dll, models/…
    └── linux-amd64/<версия>/vox, libonnxruntime.so, models/…
```

SSH-доступ к узлу — `build/remote/<node>/ssh.conf`.

## Порты

| Порт | Назначение |
|---|---|
| 9741 | Signal WS + `/api/update/*` + `/health`, `/healthz` |
| 9742 | Debug HTTP сигналки (только localhost, если задан `DEBUG_PORT`) |
| 9750 | Debug logs API (`server/debug`, если запущен) |
| 9760 | SFU (WS) |
| 3478 | TURN (coturn) |
