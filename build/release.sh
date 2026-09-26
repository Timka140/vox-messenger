#!/usr/bin/env bash
# release.sh — собирает подписанный релиз Vox и загружает на signal server.
#
# Usage:
#   ./release.sh v0.00.000.0001 "Changelog message" [force]
#
# Требования:
#   - .local/release-sign.key (private Ed25519, в git не попадает)
#   - build/release-sign.pub  (public base64)
#   - CGO toolchain (MinGW для Windows, GCC для Linux)
#   - ssh/scp доступ ко всем узлам из RELEASE_NODES (build/nodes/<name>.conf)
#
# Env:
#   RELEASE_NODES  узлы для выкладки (по умолчанию "vps1 vps2")
#   KEEP_RELEASES  сколько версий хранить (по умолчанию 3)
#   DRY_RUN=1      собрать и смерджить, но на узлы ничего не заливать
#
# Недоступный узел не валит выкладку на остальные, но скрипт выходит с кодом 1
# и печатает команду догонки: ./build/sync-releases.sh <node> <src>

set -euo pipefail

if [ $# -lt 2 ]; then
    echo "usage: $0 <version> <changelog> [force]"
    echo "  version format: v0.00.000.0001"
    exit 2
fi

VERSION="$1"
CHANGELOG="$2"
FORCE="${3:-false}"

# Validate version format
if ! echo "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]{2}\.[0-9]{3}\.[0-9]{4}$'; then
    echo "error: bad version format '$VERSION' — expected v0.00.000.0000"
    exit 1
fi

# Paths
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
APP_DIR="$REPO_ROOT/app"

PRIV_KEY="$REPO_ROOT/.local/release-sign.key"
PUB_KEY="$REPO_ROOT/build/release-sign.pub"

if [ ! -f "$PRIV_KEY" ]; then
    echo "error: missing private signing key at $PRIV_KEY"
    echo "generate with: cd app && go run ./cmd/sign-release -gen-key ../.local/release-sign && mv ../.local/release-sign.pub ../build/"
    exit 1
fi
if [ ! -f "$PUB_KEY" ]; then
    echo "error: missing public signing key at $PUB_KEY"
    exit 1
fi

# Git clean check (optional but recommended)
if [ -n "$(cd "$REPO_ROOT" && git status --porcelain 2>/dev/null || true)" ]; then
    echo "warning: working tree is dirty"
    read -p "continue anyway? [y/N] " ans
    # strip trailing CR (WSL / Windows terminal может отдать 'y\r')
    ans="${ans%$'\r'}"
    case "$ans" in
        y|Y) ;;
        *) echo "aborted (got: ${ans@Q})"; exit 1 ;;
    esac
fi

PUBKEY_B64="$(cat "$PUB_KEY" | tr -d '\n\r ')"
OUT="$REPO_ROOT/.local/release/$VERSION"
mkdir -p "$OUT"

echo "=== Building libnx_df (DeepFilterNet3 Rust staticlib) ==="
# Статическая библиотека third_party/libdf линкуется в app через тег nx_dfn
# (см. app/internal/audio/audio_dfn.go). Для windows-amd64 собираем через
# x86_64-pc-windows-gnu кросс-таргет Rust. Для linux — нативный release.
# Без nx_dfn приложение всё равно соберётся, но NSDeep упадёт до RNNoise.
LIBDF_MANIFEST="$REPO_ROOT/third_party/libdf/Cargo.toml"
if [ -f "$LIBDF_MANIFEST" ] && command -v cargo >/dev/null 2>&1; then
    echo "--- cargo build x86_64-pc-windows-gnu ---"
    # RUST_MIN_STACK=32MB нужен для fat-LTO в nx_df staticlib — без этого
    # rustc падает с SIGSEGV на кросс-компиляции в windows-gnu (сам rustc
    # в error message предлагает эту переменную).
    RUST_MIN_STACK=33554432 cargo build --release --target x86_64-pc-windows-gnu --manifest-path "$LIBDF_MANIFEST"
    # echo "--- cargo build native (linux) ---"
    # cargo build --release --manifest-path "$LIBDF_MANIFEST"
    DFN_TAG="nx_dfn "
else
    echo "warning: cargo not found or third_party/libdf missing — building without DFN3 support"
    DFN_TAG=""
fi

echo "=== Building sign-release tool ==="
SIGN="$REPO_ROOT/build/out/sign-release"
mkdir -p "$(dirname "$SIGN")"
(cd "$APP_DIR" && CGO_ENABLED=0 go build -o "$SIGN" ./cmd/sign-release)

echo "=== Building manifest-merge tool ==="
MERGE="$REPO_ROOT/build/out/manifest-merge"
(cd "$APP_DIR" && CGO_ENABLED=0 go build -o "$MERGE" ./cmd/manifest-merge)

build_platform() {
    local goos="$1"
    local goarch="$2"
    local ext="$3"
    local platform="${goos}-${goarch}"
    local platdir="$OUT/$platform"
    mkdir -p "$platdir"

    local binname="vox${ext}"
    local binpath="$platdir/$binname"

    echo "=== Building $platform ==="
    local ldflags="-H windowsgui -X main.AppVersion=$VERSION -X main.UpdatePublicKey=$PUBKEY_B64"
    if [ "$goos" != "windows" ]; then
        ldflags="-X main.AppVersion=$VERSION -X main.UpdatePublicKey=$PUBKEY_B64"
    fi

    local cc=""
    if [ "$goos" = "windows" ] && [ "$(uname -s)" != "MINGW"* ] && [ "$(uname -s)" != "MSYS"* ]; then
        # CXX обязателен: internal/codec/h264enc собирает C++-bridge (openh264);
        # без него cgo берёт линуксовый g++ и падает на «-mthreads».
        cc="CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++"
    fi

    env $cc CGO_ENABLED=1 GOOS=$goos GOARCH=$goarch \
        bash -c "cd '$APP_DIR' && go build -tags '${DFN_TAG}nx_rnnoise nx_onnx' -ldflags \"$ldflags\" -o '$binpath' ." || {
        echo "warning: build failed for $platform, skipping"
        rm -rf "$platdir"
        return 0
    }

    # Копируем DFN3 модель в <platform>/models/ — manifest-merge обойдёт
    # папку рекурсивно и добавит её в bundle.files[]. Клиент скачает и
    # разместит рядом с exe через findDFModelPath → <exeDir>/models/.
    MODEL_SRC="$REPO_ROOT/.local/models/DeepFilterNet3.tar.gz"
    if [ -n "$DFN_TAG" ] && [ -f "$MODEL_SRC" ]; then
        mkdir -p "$platdir/models"
        cp "$MODEL_SRC" "$platdir/models/"
        echo "shipped asset: $platdir/models/DeepFilterNet3.tar.gz"
    fi

    # Silero VAD модель (Этап 3 голосового трафика). Без неё VAD-тумблер
    # в UI работает, но loadVADDetectorAsync логирует "VAD model not found".
    VAD_MODEL="$REPO_ROOT/.local/models/VAD/silero_vad.onnx"
    if [ -f "$VAD_MODEL" ]; then
        mkdir -p "$platdir/models/VAD"
        cp "$VAD_MODEL" "$platdir/models/VAD/"
        echo "shipped asset: $platdir/models/VAD/silero_vad.onnx"
    else
        echo "warning: Silero VAD model not found at $VAD_MODEL — VAD won't work for users"
    fi

    # onnxruntime.dll (для nx_onnx): Silero VAD грузит её динамически через
    # SetSharedLibraryPath. Без DLL app стартует, но VAD остаётся в off-state.
    if [ "$goos" = "windows" ]; then
        ORT_LIB="$APP_DIR/internal/ml/onnx/lib/windows-amd64/onnxruntime.dll"
        if [ -f "$ORT_LIB" ]; then
            cp "$ORT_LIB" "$platdir/"
            echo "shipped asset: $platdir/onnxruntime.dll"
        else
            echo "warning: onnxruntime.dll not found at $ORT_LIB — VAD won't work for Windows users"
        fi
    fi

    echo "built: $binpath"
}

build_platform windows amd64 .exe
# build_platform linux   amd64 ""

# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"
RELEASE_NODES="${RELEASE_NODES:-vps1 vps2}"

# manifest_latest <file> — поле "latest" манифеста (без jq: в WSL его нет).
manifest_latest() {
    grep -o '"latest"[[:space:]]*:[[:space:]]*"[^"]*"' "$1" 2>/dev/null | head -1 | sed 's/.*"\([^"]*\)"$/\1/'
}

# Pull existing manifests from all nodes. База для merge — манифест с самой
# высокой latest. Узел без манифеста считается пустым; узел, недоступный
# по SSH, пропускается и попадает в FAILED.
echo "=== Fetching existing manifests ($RELEASE_NODES) ==="
EXISTING="$OUT/existing.json"
REACHABLE=()
FAILED=()
BEST_NODE=""
BEST_LATEST=""
declare -A NODE_LATEST=()
for n in $RELEASE_NODES; do
    load_node "$n"
    if ! node_reachable; then
        echo "warning: узел $n ($REMOTE) недоступен — пропускаю"
        FAILED+=("$n")
        continue
    fi
    REACHABLE+=("$n")
    f="$OUT/existing-$n.json"
    rm -f "$f"
    latest=""
    if node_get "$REMOTE_DIR/releases/manifest.json" "$f" 2>/dev/null; then
        latest="$(manifest_latest "$f")"
    fi
    NODE_LATEST[$n]="${latest:-none}"
    echo "  $n: latest=${latest:-нет манифеста}"
    if [ -n "$latest" ]; then
        if [ -z "$BEST_LATEST" ] || { [ "$latest" != "$BEST_LATEST" ] && \
            [ "$(printf '%s\n%s\n' "$BEST_LATEST" "$latest" | sort -V | tail -1)" = "$latest" ]; }; then
            BEST_LATEST="$latest"
            BEST_NODE="$n"
        fi
    fi
done

if [ ${#REACHABLE[@]} -eq 0 ]; then
    echo "error: ни один узел из RELEASE_NODES не доступен"
    exit 1
fi

if [ -n "$BEST_NODE" ]; then
    cp "$OUT/existing-$BEST_NODE.json" "$EXISTING"
    echo "база для merge: манифест $BEST_NODE ($BEST_LATEST)"
    for n in "${REACHABLE[@]}"; do
        if [ "${NODE_LATEST[$n]}" != "$BEST_LATEST" ]; then
            echo "warning: на $n latest=${NODE_LATEST[$n]} — старые версии там отсутствуют;"
            echo "         после релиза: ./build/sync-releases.sh $n $BEST_NODE"
        fi
    done
else
    echo "no existing manifest on nodes, starting fresh"
    echo '{"version":2,"releases":[]}' > "$EXISTING"
fi

# Merge: сканирует OUT/<platform>/ рекурсивно, собирает files[] с sha256
echo "=== Merging manifest (v2) ==="
FORCE_FLAG=""
if [ "$FORCE" = "true" ] || [ "$FORCE" = "force" ]; then
    FORCE_FLAG="-force"
fi

KEEP="${KEEP_RELEASES:-3}"
PRUNED_LIST="$OUT/pruned.txt"

"$MERGE" \
    -existing "$EXISTING" \
    -out "$OUT/manifest.json" \
    -version "$VERSION" \
    -changelog "$CHANGELOG" \
    $FORCE_FLAG \
    -keep "$KEEP" \
    -pruned-out "$PRUNED_LIST" \
    -artifacts "$OUT"

# Подписываем manifest.json → manifest.json.sig (Ed25519 над sha256 файла).
# В новой схеме это единственная подпись — индивидуальные .sig на бинари больше
# не генерятся, целостность каждого файла гарантируется sha256 внутри манифеста,
# а целостность всего манифеста — этой подписью.
echo "=== Signing manifest ==="
"$SIGN" -key "$PRIV_KEY" -in "$OUT/manifest.json" -out "$OUT/manifest.json.sig"
echo "signed: $OUT/manifest.json.sig"

# upload_to_node — заливает артефакты и manifest на текущий узел (после
# load_node), затем reconcile. Возвращает 1 при любой ошибке (set -e внутри
# функции, вызванной из if, не действует — отсюда явные || return 1).
upload_to_node() {
    local rel="$REMOTE_DIR/releases"
    local platform platdir
    node_ssh "mkdir -p $rel/windows-amd64/$VERSION $rel/linux-amd64/$VERSION" || return 1

    for platform in windows-amd64 linux-amd64; do
        platdir="$OUT/$platform"
        if [ -d "$platdir" ] && [ "$(ls -A "$platdir" 2>/dev/null)" ]; then
            echo "uploading $platform..."
            # -r нужен потому что в bundle могут быть подпапки (models/, runtime/, ...)
            node_put_r "$rel/$platform/$VERSION/" "$platdir"/* || return 1
        fi
    done

    # manifest — последним: клиент не увидит версию, пока файлы не залиты.
    echo "uploading manifest.json + signature..."
    node_put "$rel/" "$OUT/manifest.json" "$OUT/manifest.json.sig" || return 1

    # Reconcile releases dir with manifest: удаляем всё, чего нет в свежем
    # manifest.json. Покрывает и prune (-keep), и «сироты» от старых релизов,
    # когда manifest пересобирался с нуля и терял записи директорий на диске.
    # Скрипт передаётся here-string, а не конвейером: при pipefail конвейер
    # падал бы по SIGPIPE, если читатель (DRY_RUN) не забирает stdin.
    echo "reconciling releases dir with manifest..."
    local script
    script="set -e
cd $rel
$(cat <<'REMOTE'
KEEP=$(jq -r '.releases[].version' manifest.json | sort -u)
for platform in windows-amd64 linux-amd64; do
    [ -d "$platform" ] || continue
    for dir in "$platform"/v*/; do
        [ -d "$dir" ] || continue
        ver=$(basename "$dir")
        if ! grep -qx "$ver" <<<"$KEEP"; then
            echo "  removing orphan $platform/$ver"
            rm -rf "$dir"
        fi
    done
done
REMOTE
)"
    node_ssh bash -s <<<"$script" || return 1
}

UPLOADED=()
for n in "${REACHABLE[@]}"; do
    load_node "$n"
    echo "=== Uploading to $n ($REMOTE:$REMOTE_DIR) ==="
    if upload_to_node; then
        UPLOADED+=("$n")
    else
        echo "error: выкладка на $n не удалась"
        FAILED+=("$n")
    fi
done

echo ""
echo "=== Release $VERSION complete ==="
echo "Files at: $OUT"
echo "Nodes:    ${UPLOADED[*]:-нет}"
echo "Kept:     last $KEEP releases (override via KEEP_RELEASES env)"

if [ ${#FAILED[@]} -gt 0 ]; then
    echo ""
    echo "error: релиз не дошёл до узлов: ${FAILED[*]}"
    if [ ${#UPLOADED[@]} -gt 0 ]; then
        for n in "${FAILED[@]}"; do
            echo "  догнать: ./build/sync-releases.sh $n ${UPLOADED[0]}"
        done
    fi
    exit 1
fi
