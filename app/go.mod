module vox_app

go 1.26.1

// Патч pion: fix access violation (0xc0000005) при channel operations на Windows.
// GC освобождает память каналов пока горутины блокированы в select/chanrecv.
// Добавлен runtime.KeepAlive для предотвращения преждевременного GC.
replace (
	github.com/pion/transport/v4 => ../third_party/pion-transport
	github.com/pion/turn/v4 => ../third_party/pion-turn
)

require (
	filippo.io/edwards25519 v1.2.0
	git.sr.ht/~jackmordaunt/go-toast v1.1.2
	github.com/fatih/color v1.19.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/ilyakaznacheev/cleanenv v1.5.0
	github.com/kbinani/screenshot v0.0.0-20250624051815-089614a94018
	github.com/kirides/go-d3d v1.0.1
	github.com/madelynnblue/go-dsp v1.0.0
	github.com/mutecomm/go-sqlcipher/v4 v4.4.2
	github.com/pion/webrtc/v4 v4.2.11
	github.com/wailsapp/go-webview2 v1.0.23
	github.com/wailsapp/wails/v3 v3.0.0-alpha.74
	github.com/yalue/onnxruntime_go v1.27.0
	go.bug.st/serial v1.6.4
	golang.org/x/crypto v0.50.0
	golang.org/x/image v0.39.0
	golang.org/x/sys v0.43.0
	google.golang.org/protobuf v1.36.11
)

require (
	dario.cat/mergo v1.0.2 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/ProtonMail/go-crypto v1.3.0 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/bep/debounce v1.2.1 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/creack/goselect v0.1.2 // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/ebitengine/purego v0.9.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/gen2brain/shm v0.1.1 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.7.0 // indirect
	github.com/go-git/go-git/v5 v5.16.4 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	github.com/jezek/xgb v1.1.1 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/kevinburke/ssh_config v1.4.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/leaanthony/go-ansi-parser v1.6.1 // indirect
	github.com/leaanthony/u v1.1.1 // indirect
	github.com/lmittmann/tint v1.1.2 // indirect
	github.com/lxn/win v0.0.0-20210218163916-a377121e959e // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/pion/datachannel v1.6.0 // indirect
	github.com/pion/dtls/v3 v3.1.2 // indirect
	github.com/pion/ice/v4 v4.2.2 // indirect
	github.com/pion/interceptor v0.1.44 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.1 // indirect
	github.com/pion/sctp v1.9.4 // indirect
	github.com/pion/sdp/v3 v3.0.18 // indirect
	github.com/pion/srtp/v3 v3.0.10 // indirect
	github.com/pion/stun/v3 v3.1.1 // indirect
	github.com/pion/transport/v4 v4.0.1 // indirect
	github.com/pion/turn/v4 v4.1.4 // indirect
	github.com/pjbgf/sha1cd v0.5.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/samber/lo v1.52.0 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/skeema/knownhosts v1.3.2 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	golang.org/x/exp v0.0.0-20260312153236-7ab1446f8b90 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	golang.org/x/time v0.10.0 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	olympos.io/encoding/edn v0.0.0-20201019073823-d3554ca0b0a3 // indirect
)
