package services

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCallPopup struct{}

func (fakeCallPopup) Show()  {}
func (fakeCallPopup) Close() {}

// newIncomingTestService — CallsService без сети: только то, что трогает
// showIncomingCallPopup для личного звонка (createWindow, pendingInvites).
func newIncomingTestService(created *atomic.Int32) *CallsService {
	s := &CallsService{
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		pendingInvites: make(map[string]*PendingInvite),
	}
	s.createWindow = func(string, int, int, string, func()) CallPopup {
		created.Add(1)
		return fakeCallPopup{}
	}
	return s
}

// withIncomingBusy — подменить проверку полноэкранного режима и шаг опроса.
func withIncomingBusy(t *testing.T, busy *atomic.Bool) {
	prevFn, prevPoll := incomingBusyFn, incomingDeferPoll
	incomingBusyFn = busy.Load
	incomingDeferPoll = 10 * time.Millisecond
	t.Cleanup(func() { incomingBusyFn, incomingDeferPoll = prevFn, prevPoll })
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestIncomingCallNotBusyShowsNow(t *testing.T) {
	var busy atomic.Bool
	withIncomingBusy(t, &busy)
	var created atomic.Int32
	s := newIncomingTestService(&created)

	s.showIncomingCallPopup("c1", "Test", "peer", "dm", false)
	if created.Load() != 1 {
		t.Fatalf("окно создано %d раз, ждали 1 сразу", created.Load())
	}
}

func TestIncomingCallDeferredUntilNotBusy(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	withIncomingBusy(t, &busy)
	var created atomic.Int32
	s := newIncomingTestService(&created)
	s.pendingInvites["c1"] = &PendingInvite{CallID: "c1"}

	s.showIncomingCallPopup("c1", "Test", "peer", "dm", false)
	time.Sleep(50 * time.Millisecond)
	if created.Load() != 0 {
		t.Fatal("окно показано поверх полноэкранного приложения")
	}
	busy.Store(false)
	if !waitFor(t, func() bool { return created.Load() == 1 }) {
		t.Fatalf("после выхода из полноэкранного режима окно создано %d раз, ждали 1", created.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if created.Load() != 1 {
		t.Fatalf("окно создано %d раз, ждали ровно 1", created.Load())
	}
}

func TestIncomingCallDeferredDroppedWhenCallGone(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	withIncomingBusy(t, &busy)
	var created atomic.Int32
	s := newIncomingTestService(&created)
	s.pendingInvites["c1"] = &PendingInvite{CallID: "c1"}

	s.showIncomingCallPopup("c1", "Test", "peer", "dm", false)
	time.Sleep(30 * time.Millisecond)
	s.mu.Lock()
	delete(s.pendingInvites, "c1") // звонящий сбросил
	s.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	busy.Store(false)
	time.Sleep(60 * time.Millisecond)
	if created.Load() != 0 {
		t.Fatal("окно показано для уже сброшенного звонка")
	}
}
