//go:build voxdebug

package network

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Тесты glare (одновременные offer) на медиа-PC: два PeerManager'а в одном
// процессе, сигналинг закольцован in-process, ICE по loopback (voxdebug →
// policy All + loopback). Воспроизводят сценарии 2026-09-08, когда медиа-канал
// зависал навсегда в «initiateConnection пропущен: уже инициируем».
//
// Ключи: "a1…" < "b2…" ⇒ A polite, B impolite (см. HandleOffer).

// glareRouter — однонаправленный маршрутизатор с фильтром drop. Как dirRouter
// в services/screenshare_p2p_voxdebug_test.go держит порядок «SDP раньше ICE»
// (HandleICE буферизует, но проще не полагаться на это в тесте).
type glareRouter struct {
	ch   chan *Message
	mu   sync.Mutex
	drop func(*Message) bool
}

func newGlareRouter(deliver func(*Message)) *glareRouter {
	r := &glareRouter{ch: make(chan *Message, 512)}
	go func() {
		sdpSeen := map[int]bool{}
		iceBuf := map[int][]*Message{}
		for m := range r.ch {
			r.mu.Lock()
			drop := r.drop
			r.mu.Unlock()
			if drop != nil && drop(m) {
				continue
			}
			if m.Type == TypeICE && !sdpSeen[m.PcId] {
				iceBuf[m.PcId] = append(iceBuf[m.PcId], m)
				continue
			}
			deliver(m)
			if m.Type == TypeOffer || m.Type == TypeAnswer {
				sdpSeen[m.PcId] = true
				for _, bm := range iceBuf[m.PcId] {
					deliver(bm)
				}
				iceBuf[m.PcId] = nil
			}
		}
	}()
	return r
}

func (r *glareRouter) setDrop(fn func(*Message) bool) {
	r.mu.Lock()
	r.drop = fn
	r.mu.Unlock()
}

func glareDispatch(pm *PeerManager, fromPK string, m *Message) {
	switch m.Type {
	case TypeOffer:
		_ = pm.HandleOffer(fromPK, m.Payload, m.PcId)
	case TypeAnswer:
		_ = pm.HandleAnswer(fromPK, m.Payload, m.PcId)
	case TypeICE:
		_ = pm.HandleICE(fromPK, m.Payload, m.PcId)
	}
}

type glarePair struct {
	pkA, pkB   string
	pmA, pmB   *PeerManager
	aToB, bToA *glareRouter
	stateMu    sync.Mutex
	stateA     []bool // onStateChange(connected) на стороне A
}

func newGlarePair(t *testing.T) *glarePair {
	t.Helper()
	p := &glarePair{
		pkA: strings.Repeat("a1", 32),
		pkB: strings.Repeat("b2", 32),
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	noopMsg := func(string, []byte) {}
	p.aToB = newGlareRouter(func(m *Message) { glareDispatch(p.pmB, p.pkA, m) })
	p.bToA = newGlareRouter(func(m *Message) { glareDispatch(p.pmA, p.pkB, m) })
	p.pmA = NewPeerManager(logger, p.pkA,
		func(m *Message) error { p.aToB.ch <- m; return nil },
		noopMsg,
		func(_ string, connected bool) {
			p.stateMu.Lock()
			p.stateA = append(p.stateA, connected)
			p.stateMu.Unlock()
		})
	p.pmB = NewPeerManager(logger, p.pkB,
		func(m *Message) error { p.bToA.ch <- m; return nil },
		noopMsg, func(string, bool) {})
	p.pmA.SetICEServers(nil)
	p.pmB.SetICEServers(nil)
	t.Cleanup(func() {
		p.pmA.CloseAll()
		p.pmB.CloseAll()
	})
	return p
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: не дождались за %s", what, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mediaSlot — снимок media-слота пира (под локом) для assert'ов.
func mediaSlot(pm *PeerManager, peer string) (hasPC, initiating, answering bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	p, ok := pm.peers[peer]
	if !ok {
		return false, false, false
	}
	return p.PC != nil, p.Initiating, p.answering
}

// TestGlare_SimultaneousInitiate — обе стороны инициируют одновременно
// (перезапуск пира: «ПОДКЛЮЧЕНИЕ К ДРУЗЬЯМ» с обеих сторон). Ожидание: оба
// подключены, у каждого ровно один media-PC, флаги сброшены.
func TestGlare_SimultaneousInitiate(t *testing.T) {
	p := newGlarePair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = p.pmA.InitiateConnection(p.pkB) }()
	go func() { defer wg.Done(); _ = p.pmB.InitiateConnection(p.pkA) }()
	wg.Wait()

	waitUntil(t, 15*time.Second, "A connected", func() bool { return p.pmA.IsConnected(p.pkB) })
	waitUntil(t, 15*time.Second, "B connected", func() bool { return p.pmB.IsConnected(p.pkA) })

	// Флаги должны быть сброшены — иначе следующий initiate/reconnect зависнет.
	waitUntil(t, 3*time.Second, "flags cleared", func() bool {
		_, ia, aa := mediaSlot(p.pmA, p.pkB)
		_, ib, ab := mediaSlot(p.pmB, p.pkA)
		return !ia && !aa && !ib && !ab
	})
}

// TestGlare_OfferWithoutAnswer — сигналинг B→A глухой: answer (и всё
// остальное) от B до A не доходит. Раньше PC у A висел в have-local-offer
// навсегда с Initiating=true. Теперь watchdog сбрасывает слот за
// offerAnswerTimeout и шлёт onStateChange(false). У B свой путь: его
// answer-PC падает по ICE (A молчит), tryICERestart шлёт restart-offer, тот
// тоже без ответа → watchdog сбрасывает и его слот (PC не connected).
// После восстановления сигналинга повторный InitiateConnection соединяет.
func TestGlare_OfferWithoutAnswer(t *testing.T) {
	p := newGlarePair(t)
	p.bToA.setDrop(func(*Message) bool { return true })

	if err := p.pmA.InitiateConnection(p.pkB); err != nil {
		t.Fatalf("InitiateConnection: %v", err)
	}
	// Пока offer свежий — слот занят, повтор должен вернуть ErrSlotBusy.
	if err := p.pmA.InitiateConnection(p.pkB); err != ErrSlotBusy {
		t.Fatalf("повторный InitiateConnection: ожидали ErrSlotBusy, получили %v", err)
	}

	waitUntil(t, offerAnswerTimeout+5*time.Second, "watchdog сбросил слот A", func() bool {
		return !p.pmA.HasActivePeer(p.pkB)
	})
	p.stateMu.Lock()
	gotDisconnect := len(p.stateA) > 0 && !p.stateA[len(p.stateA)-1]
	p.stateMu.Unlock()
	if !gotDisconnect {
		t.Fatalf("после watchdog ожидали onStateChange(false) на A")
	}
	// B: ICE failed (~13 с) → restart-offer → watchdog (+15 с).
	waitUntil(t, 40*time.Second, "watchdog сбросил слот B (restart-offer без answer)", func() bool {
		return !p.pmB.HasActivePeer(p.pkA)
	})

	// Сигналинг «восстановился» — повторная попытка должна соединить.
	p.bToA.setDrop(nil)
	if err := p.pmA.InitiateConnection(p.pkB); err != nil {
		t.Fatalf("InitiateConnection после watchdog: %v", err)
	}
	waitUntil(t, 15*time.Second, "A connected после watchdog", func() bool { return p.pmA.IsConnected(p.pkB) })
	waitUntil(t, 15*time.Second, "B connected после watchdog", func() bool { return p.pmB.IsConnected(p.pkA) })
}

// TestGlare_StaleOfferAccepted — impolite B отправил offer, который до A не
// дошёл (пир перезапускался). Через offerGrace приходит offer от A. Раньше B
// игнорировал его («мы impolite») и обе стороны висели; теперь B видит, что
// его offer без answer устарел, и принимает входящий.
func TestGlare_StaleOfferAccepted(t *testing.T) {
	p := newGlarePair(t)
	// Offer от B до A не доходит.
	p.bToA.setDrop(func(m *Message) bool { return m.Type == TypeOffer })

	if err := p.pmB.InitiateConnection(p.pkA); err != nil {
		t.Fatalf("B InitiateConnection: %v", err)
	}
	if hasPC, init, _ := mediaSlot(p.pmB, p.pkA); !hasPC || !init {
		t.Fatalf("B: ожидали PC в have-local-offer с Initiating=true, got pc=%v init=%v", hasPC, init)
	}

	// Ждём, пока offer B перестанет быть свежим, но раньше watchdog'а.
	time.Sleep(offerGrace + 500*time.Millisecond)
	if err := p.pmA.InitiateConnection(p.pkB); err != nil {
		t.Fatalf("A InitiateConnection: %v", err)
	}
	waitUntil(t, 15*time.Second, "A connected (B принял устаревший конфликт)", func() bool { return p.pmA.IsConnected(p.pkB) })
	waitUntil(t, 15*time.Second, "B connected", func() bool { return p.pmB.IsConnected(p.pkA) })
	waitUntil(t, 3*time.Second, "flags cleared", func() bool {
		_, ia, aa := mediaSlot(p.pmA, p.pkB)
		_, ib, ab := mediaSlot(p.pmB, p.pkA)
		return !ia && !aa && !ib && !ab
	})
}
