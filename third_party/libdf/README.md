# nx_df — thin C ABI wrapper around DeepFilterNet3

Экспортирует минимальный C-интерфейс поверх Rust crate `deep_filter` (v0.5.6 —
та же версия, что и `tools/ns-reference/pyref/deep-filter.exe`). Используется из Go через
cgo, пакет `app/internal/audio/dfnative` (build tag `nx_dfn`).

## Сборка

Требуется Rust toolchain (`rustup`). Из WSL — как и `release.sh`.

```bash
# Linux (для VPS / разработки)
cargo build --release --manifest-path third_party/libdf/Cargo.toml
# → third_party/libdf/target/release/libnx_df.a + libnx_df.so

# Windows (из WSL)
rustup target add x86_64-pc-windows-gnu
cargo build --release --manifest-path third_party/libdf/Cargo.toml \
    --target x86_64-pc-windows-gnu
# → third_party/libdf/target/x86_64-pc-windows-gnu/release/libnx_df.a + nx_df.dll
```

Статическая линковка (`libnx_df.a`) предпочтительнее — не надо таскать
отдельный .dll/.so с релизом. Go cgo подхватит через `#cgo LDFLAGS`.

## API

См. `include/nx_df.h`. Контракт:

- `nx_df_create(models_dir)` — путь к папке с `config.ini`, `enc.onnx`,
  `erb_dec.onnx`, `df_dec.onnx`. Возвращает `NULL` при ошибке.
- `nx_df_frame_len(state)` — 480 для DFN3 (48 kHz, 10 ms hop).
- `nx_df_process(state, in_out, len)` — in-place, моно float32 [-1, 1].
- `nx_df_destroy(state)` — освободить.

**Потокобезопасность:** state НЕ shared между goroutines без внешней
синхронизации.

## Проверка

После сборки прогнать `tools/ns-reference/noisy_raw_48k.wav` через новый `dfnative` и
сравнить с `tools/ns-reference/reference_out/` — должно сойтись с точностью до float32
round-off (тот же crate, что `deep-filter.exe`).

## NB по API crate

Сигнатуры `DfTract::new`, `DfParams`, `RuntimeParams` я писал по памяти для
0.5.6. Если `cargo build` упадёт на именах типов — свериться с
`https://crates.io/crates/deep_filter/0.5.6` и подправить `src/lib.rs`. Логика
обёртки (in-place 480-sample frame, hop_size из tract) останется той же.
