//go:build voxdebug

package network

import "github.com/pion/webrtc/v4"

// iceTransportPolicy для voxdebug-сборки: All — позволяет host-кандидаты
// (127.0.0.1, LAN), чтобы две локальные копии (bin/, bin2/) на одном ПК
// нашли прямую пару и не гнали трафик через TURN-VPS. Production остаётся
// на Relay (см. ice_policy_release.go и peermgr.go createPeerConnection).
//
// Скорость на одном ПК: с Relay через VPS ~1 MB/s (упирается в uplink юзера
// + пропускную способность канала VPS), с All на host — гигабит локально.
func iceTransportPolicy() webrtc.ICETransportPolicy {
	return webrtc.ICETransportPolicyAll
}

// allowLoopbackIP — true для voxdebug: две локальные копии находят друг
// друга через 127.0.0.1, ICE выбирает host-пару, скорость гигабитная.
func allowLoopbackIP() bool { return true }
