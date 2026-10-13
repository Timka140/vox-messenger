# Vox

**English** · [Русский](README.ru.md) · Website: [northwindcdn.com/vox](https://northwindcdn.com/vox/)

A peer-to-peer messenger for small groups of friends: messages, voice channels, calls with screen sharing and files travel directly between participants over WebRTC. The signaling server helps participants find each other, and relays traffic itself only for large groups and voice rooms.

Download for Windows and Linux: [northwindcdn.com/vox](https://northwindcdn.com/vox/). Run your own server: [northwindcdn.com/vox/server](https://northwindcdn.com/vox/server/).

## Features

- **Direct messages and groups** — text, voice messages, files, P2P over DataChannel
- **Voice channels and calls** — Opus, noise suppression (RNNoise, DeepFilterNet3, FastEnhancer), SFU for large rooms
- **Screen sharing**
- **Offline delivery** — a direct message waits on the sender's machine and goes out when the recipient comes online; messages of large groups are stored by the signaling server cluster
- **Your profile is an Ed25519 key**, created on your computer and recoverable from a 12-word phrase; the local database is encrypted (SQLCipher)
- **Auto-update** — releases are signed with Ed25519
- **Meshtastic** — experimental transport over LoRa

## Security

The profile key (Ed25519) is created on your computer; it signs everything that proves identity: server login, SDP of P2P connections, group messages.

| Channel | Protection |
|---|---|
| Direct messages, files, calls, screen sharing (P2P) | WebRTC DTLS; the SDP carrying the DTLS fingerprint is signed by the sender's key, so the server cannot become a man in the middle. TURN does not decrypt traffic |
| Server-relayed groups (50+ members or by setting) | End-to-end encryption: XChaCha20-Poly1305, the message key is wrapped for each member (NaCl box), the author signs. The server stores only ciphertext, for up to 7 days. A new member reads the history from the moment they joined |
| Voice rooms via SFU | End-to-end encryption of frames with ChaCha20-Poly1305 under the sender's key; keys are exchanged over P2P and rotated when someone leaves |
| Client ↔ signaling server and SFU | TLS (`wss://`, port 9743, SFU 9762). The node certificate is self-signed; the node confirms its fingerprint with its key during login |
| Between cluster nodes | TLS with the same key-based confirmation |
| Local database and attachments | SQLCipher and AES-256-GCM; the key is stored next to the database (`vox.key`) |
| LoRa (Meshtastic, experimental) | End-to-end NaCl box encryption with profile keys |

Not provided: forward secrecy for server-relayed groups — a leaked profile key exposes the messages stored on the server within the retention period; the server sees metadata (who is in which group, when and how much they write).

Updates are signed: the client installs only what is listed in a manifest signed with Ed25519, and checks the SHA-256 of every file.

## Stack

| Component | Technology |
|-----------|-----------|
| Client | Go 1.26, Wails v3, vanilla JS (ES modules) |
| Database | SQLCipher |
| P2P | Pion WebRTC v4 (transport/turn forks in `third_party/`) |
| Servers | Go: signaling + cluster, SFU, debug logs |

## Repository layout

```
app/                  client (Wails): Go backend, frontend/, cmd/ — release and noise suppression tools
server/
  signal/             signaling server, node cluster, releases
  sfu/                SFU for voice
  debug/              client log collector
third_party/
  pion-transport/     fork of github.com/pion/transport/v4 (replace in app/go.mod)
  pion-turn/          fork of github.com/pion/turn/v4
  opus/               static libopus for cgo
  libdf/              DeepFilterNet wrapper (Rust staticlib for cgo)
build/                build, release, node deployment (nodes/, remote/)
tools/
  meshtastic/         python tools for debugging Heltec/Meshtastic
  ns-reference/       reference wav files and python reference of the noise suppressor
  wordlist-ru/        generator of the Russian recovery phrase wordlist
docs/                 build and releases, cluster contract (in Russian)
.local/               local only, not in git: logs, models, built releases, signing key
```

## Building

Details — [docs/build.md](docs/build.md) (in Russian). In short:

```bash
git clone --recursive https://github.com/Timka140/vox-messenger.git
# or after a plain clone:
git submodule update --init app/internal/audio/rnnoise

# Development client (needs gcc and g++: MinGW on Windows;
# on Linux also libgtk-3-dev and libwebkit2gtk-4.1-dev)
cd app && CGO_ENABLED=1 go build -o vox.exe .

# Signaling server for Linux
./build/signal-linux.sh
```

This build has no noise suppression, VAD or auto-update (version `dev`). A full build with the `nx_rnnoise nx_onnx nx_dfn` tags requires downloaded models and onnxruntime — see docs/build.md.

## Tests

```bash
cd app && go test ./...
cd server/signal && go test ./...
```

## License

[MIT](LICENSE). Third-party code is distributed under its own licenses: pion forks (`third_party/pion-*`, MIT), libopus (`third_party/opus`, BSD, text in the headers), RNNoise (`app/internal/audio/rnnoise`, BSD), OpenH264 (`app/internal/codec/h264enc`, BSD), miniaudio (public domain / MIT-0), FastEnhancer and GTCRN models (MIT, see the README in their directories).
