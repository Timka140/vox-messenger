package services

import (
	"log/slog"
	"sync"
)

// EventHandler — обработчик события из EventBus. Payload приходит как any:
// тип для каждого топика описан в комментарии рядом с константой TopicXxx.
// Хендлер должен делать type assertion и молча пропускать payload неверного типа.
type EventHandler func(payload any)

// EventBus — простой in-process pub/sub для развязки сервисов.
// NetworkService эмитит события (peer online/offline, p2p ready, latest release),
// остальные сервисы подписываются без прямого знакомства друг с другом.
//
// Контракт:
//   - Emit вызывает всех подписчиков синхронно в том же потоке.
//     Если хендлер тяжёлый — он сам должен запустить горутину.
//   - Паника внутри хендлера ловится recover'ом и логируется; остальные
//     подписчики того же события продолжают получать уведомление.
//   - Subscribe можно вызывать в любой момент; порядок подписок сохраняется.
type EventBus struct {
	log  *slog.Logger
	mu   sync.RWMutex
	subs map[string][]EventHandler
}

func NewEventBus(log *slog.Logger) *EventBus {
	return &EventBus{
		log:  log.With(slog.String("component", "eventbus")),
		subs: make(map[string][]EventHandler),
	}
}

// Subscribe добавляет хендлер на топик. Возвращаемого значения нет — отписка
// не поддерживается (в наших use-case'ах подписки живут весь срок сервисов).
func (b *EventBus) Subscribe(topic string, handler EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], handler)
}

// Emit отправляет payload всем подписчикам. Безопасно вызывать из горутины
// эмиттера; подписчики выполняются в том же потоке по порядку подписки.
func (b *EventBus) Emit(topic string, payload any) {
	b.mu.RLock()
	handlers := make([]EventHandler, len(b.subs[topic]))
	copy(handlers, b.subs[topic])
	b.mu.RUnlock()

	for _, h := range handlers {
		b.safeCall(topic, h, payload)
	}
}

func (b *EventBus) safeCall(topic string, h EventHandler, payload any) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("event handler panic",
				slog.String("topic", topic),
				slog.Any("recover", r))
		}
	}()
	h(payload)
}

// --- Топики ---
//
// Правила:
//   - Константа именуется Topic<Subject><Event>.
//   - Тип payload описан в комментарии.
//   - Новые топики добавлять сюда, не распыляя по коду.

const (
	// TopicPeerOnline — пир появился онлайн в сигнальной сети (peer_online от signal server).
	// Payload: string — peerID (hex pubKey).
	TopicPeerOnline = "peer.online"

	// TopicPeerOffline — пир ушёл офлайн в сигнальной сети (peer_offline от signal server).
	// Payload: string — peerID.
	TopicPeerOffline = "peer.offline"

	// TopicP2PReady — P2P DataChannel с пиром открыт и готов к отправке.
	// Эмитится один раз при onDataChannelReady.
	// Payload: string — pubKey пира.
	TopicP2PReady = "peer.p2p_ready"

	// TopicAttachReady — "attachments" DataChannel (files PC) с пиром открыт.
	// Files PC поднимается лениво и позже media PC; manifest, не ушедший на
	// TopicP2PReady из-за «files PC not ready», доставляется по этому топику.
	// Payload: string — pubKey пира.
	TopicAttachReady = "peer.attach_ready"

	// TopicLatestRelease — signal server прислал информацию о последнем релизе
	// (в handshake auth_ok).
	// Payload: *network.LatestReleaseInfo.
	TopicLatestRelease = "update.latest_release"
)
