//go:build !nx_voicedebug

// Release-сборка voice service: probe-инструментация отключена.
// Go compiler eliminate всё, что находится внутри `if voiceDebug { ... }`
// когда const равен false (dead code elimination на уровне SSA).

package services

const voiceDebug = false
