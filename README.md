# Vox

Защищённый P2P мессенджер с end-to-end шифрованием. Сообщения, голос и файлы идут напрямую между участниками через WebRTC; сигнальный сервер нужен для обмена метаданными подключения, офлайн-доставки и больших групп.

## Возможности

- **Личные сообщения и группы** — текст, голосовые, файлы, P2P через DataChannel
- **Голосовые каналы и звонки** — Opus, шумоподавление (RNNoise, DeepFilterNet3, FastEnhancer), SFU для больших комнат
- **Демонстрация экрана**
- **Офлайн-доставка** — через кластер сигнальных серверов
- **Шифрование** — Ed25519 identity, SQLCipher (зашифрованная БД), DTLS (WebRTC)
- **Автообновление** — релизы подписаны Ed25519
- **Meshtastic** — экспериментальный транспорт через LoRa

## Технологии

| Компонент | Технология |
|-----------|-----------|
| Клиент | Go 1.26, Wails v3, Vanilla JS (ES modules) |
| БД | SQLCipher |
| P2P | Pion WebRTC v4 (форки transport/turn в `third_party/`) |
| Серверы | Go: сигналка + кластер, SFU, debug-логи |

## Структура репозитория

```
app/                  клиент (Wails): Go-бэкенд, frontend/, cmd/ — утилиты релиза и шумодава
server/
  signal/             сигнальный сервер, кластер узлов, релизы
  sfu/                SFU для голоса
  debug/              приём логов клиентов
third_party/
  pion-transport/     форк github.com/pion/transport/v4 (replace в app/go.mod)
  pion-turn/          форк github.com/pion/turn/v4
  opus/               статическая libopus для cgo
  libdf/              обёртка DeepFilterNet (Rust staticlib для cgo)
build/                сборка, релиз, деплой на узлы (nodes/, remote/)
tools/
  meshtastic/         python-утилиты для отладки Heltec/Meshtastic
  ns-reference/       эталонные wav и python-референс шумодава
  wordlist-ru/        генератор русского словаря секретной фразы
docs/                 сборка и релизы, контракт кластера
.local/               локально, не в git: логи, модели, собранные релизы, ключ подписи
```

## Сборка

Подробно — [docs/build.md](docs/build.md). Коротко:

```bash
# Клиент (нужен MinGW/GCC для cgo)
cd app && CGO_ENABLED=1 go build .

# Сигнальный сервер под Linux
./build/signal-linux.sh
```

Модели шумодава (`DeepFilterNet3.tar.gz`, `VAD/silero_vad.onnx`) кладутся в `.local/models/`.

## Тесты

```bash
cd app && go test ./...
cd server/signal && go test ./...
```

## Лицензия

[MIT](LICENSE). Сторонние компоненты в `third_party/` и `app/internal/audio/rnnoise` распространяются под своими лицензиями (файлы LICENSE в их каталогах).
