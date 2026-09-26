//go:build !voxdebug

package network

import "github.com/pion/webrtc/v4"

// iceTransportPolicy для production: Relay — весь P2P-трафик через TURN.
//
// История: в v0.0068 пробовали All (direct + fallback на TURN) в надежде что
// патчи pion-transport/pion-turn закрыли DTLS-loss за CGNAT. Логи v0.0071
// показали обратное: на реальных юзерских NAT прямая host/srflx-пара ICE
// check проходит, DC отчитывается open, но через 5-7 сек NAT-маппинг тает,
// consent freshness падает и PC рвётся. voice_join при этом успевает уйти в
// SCTP без ошибки, но не доезжает — звонок есть, голоса нет.
// Hardcoded Relay как в v0.0058-0067 — медленнее (упирается в ~1 MB/s через
// VPS), но реально работает. LAN-скорость даёт только voxdebug-сборка для
// тестов на одной машине.
func iceTransportPolicy() webrtc.ICETransportPolicy {
	return webrtc.ICETransportPolicyRelay
}

// allowLoopbackIP — false для production: 127.x.x.x не используется в P2P
// между разными машинами. Только voxdebug-сборка разрешает (две копии на одном ПК).
func allowLoopbackIP() bool { return false }
