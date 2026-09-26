//go:build nx_voicedebug

// Debug-сборка voice service: включает probe-инструментацию.
//   - wire-latency замер через DFNP header (send→recv)
//   - mix queue dwell time (recv→playback.Write)
//   - mix queue peak length
//   - dfn perf monitor (avg/max/over_budget frame time в RT callback)
//   - расширенные поля в VOICE PROFILE логе
//
// Включается через тег nx_voicedebug в .vscode/launch.json для delve-сессий.
// Release-сборка (build/release.sh) собирается БЕЗ этого тега — все probe
// дают zero overhead за счёт compile-time dead-code elimination (const bool).

package services

const voiceDebug = true
