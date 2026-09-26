package network

import (
	"fmt"
	"log/slog"

	"github.com/pion/webrtc/v4"
)

// Звук демонстрации экрана (фаза 5e): отдельный DataChannel "screen-audio"
// на screen PC — unordered, без ретрансмитов. Звук реального времени не
// должен стоять в очереди за 60-КиБ кусками видео надёжного канала "screen".
// Канал добавляется на уже соединённый PC: SCTP открывает новый поток сам,
// повторного согласования SDP не нужно.
//
// Открывается только пирам, приславшим caps screenAudio: у старых клиентов
// DataChannel с незнакомым label попадает в default-ветку OnDataChannel и
// затирает DC сообщений.

// screenAudioBufferLimit — выше этого буферизованного объёма пакет звука
// отбрасывается: поздний звук хуже потерянного (как у голоса).
const screenAudioBufferLimit = 64 * 1024

// SetOnScreenAudio — callback входящих пакетов звука демонстрации.
func (pm *PeerManager) SetOnScreenAudio(fn OnP2PMessage) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onScreenAudio = fn
}

// EnsureScreenAudioDC — канал звука на уже поднятом screen PC (сторона
// демонстрирующего). Идемпотентен. Без screen PC — ошибка: его поднимает видео.
func (pm *PeerManager) EnsureScreenAudioDC(remotePubKey string) error {
	pm.mu.Lock()
	peer, ok := pm.peers[remotePubKey]
	if !ok || peer.ScreenPC == nil {
		pm.mu.Unlock()
		return fmt.Errorf("нет screen PC с %s", safeShort(remotePubKey))
	}
	if peer.ScreenAudioDC != nil {
		pm.mu.Unlock()
		return nil
	}
	pc := peer.ScreenPC
	pm.mu.Unlock()

	ordered := false
	var retransmits uint16
	dc, err := pc.CreateDataChannel(ScreenAudioDataChannelLabel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &retransmits,
	})
	if err != nil {
		return fmt.Errorf("create screen-audio dc: %w", err)
	}
	pm.setupScreenAudioDataChannel(remotePubKey, dc)

	pm.mu.Lock()
	if p, ok := pm.peers[remotePubKey]; ok && p.ScreenPC == pc && p.ScreenAudioDC == nil {
		p.ScreenAudioDC = dc
		pm.mu.Unlock()
		return nil
	}
	pm.mu.Unlock()
	// Слот сменился (или канал уже создан параллельно), пока мы создавали.
	_ = dc.Close()
	return nil
}

// SendScreenAudio — пакет звука демонстрации. При переполненном буфере
// молча отбрасывается.
func (pm *PeerManager) SendScreenAudio(remotePubKey string, data []byte) error {
	pm.mu.RLock()
	var dc *webrtc.DataChannel
	if peer, ok := pm.peers[remotePubKey]; ok {
		dc = peer.ScreenAudioDC
	}
	pm.mu.RUnlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("screen-audio DC с %s не открыт", safeShort(remotePubKey))
	}
	if dc.BufferedAmount() > screenAudioBufferLimit {
		return nil
	}
	return dc.Send(data)
}

func (pm *PeerManager) setupScreenAudioDataChannel(remotePubKey string, dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		pm.log.Info("Screen-audio DataChannel открыт", slog.String("peer", safeShort(remotePubKey)))
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		pm.mu.RLock()
		fn := pm.onScreenAudio
		pm.mu.RUnlock()
		if fn != nil {
			fn(remotePubKey, msg.Data)
		}
	})
}
