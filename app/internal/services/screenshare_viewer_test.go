package services

import (
	"io"
	"log/slog"
	"testing"
)

type fakeViewer struct{ shown, closed, focused int }

func (f *fakeViewer) Show()               { f.shown++ }
func (f *fakeViewer) Close()              { f.closed++ }
func (f *fakeViewer) Focus()              { f.focused++ }
func (f *fakeViewer) SetAlwaysOnTop(bool) {}
func (f *fakeViewer) ToggleFullscreen()   {}
func (f *fakeViewer) Minimise()           {}
func (f *fakeViewer) ToggleMaximise()     {}

// Окна демонстрации — по одному на пира (фаза 7e): открытие второго не
// закрывает первое, повторное поднимает открытое, закрытие — адресное.
func TestViewerWindowsPerPeer(t *testing.T) {
	s := &ScreenShareService{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		network: &NetworkService{}, // без emit — события никуда не уходят
	}
	var wins []*fakeViewer
	var slots []int
	closers := map[string]func(){}
	s.SetViewerWindowFactory(func(peerID, title, query string, slot int, onClosed func()) ViewerWindow {
		w := &fakeViewer{}
		wins = append(wins, w)
		slots = append(slots, slot)
		closers[peerID] = onClosed
		return w
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(s.OpenViewerWindow("A", "a", "", 0, "dark"))
	must(s.OpenViewerWindow("B", "b", "", 0, "dark"))
	if len(wins) != 2 || wins[0].closed != 0 {
		t.Fatalf("два пира: окон %d, первое закрыто %d раз", len(wins), wins[0].closed)
	}
	if slots[0] != 0 || slots[1] != 1 {
		t.Fatalf("slots = %v, want [0 1]", slots)
	}

	must(s.OpenViewerWindow("A", "a", "", 0, "dark"))
	if len(wins) != 2 || wins[0].focused != 1 {
		t.Fatalf("повторно A: окон %d, focus %d", len(wins), wins[0].focused)
	}

	oldA := closers["A"]
	must(s.CloseViewerWindow("A"))
	if wins[0].closed != 1 || wins[1].closed != 0 || len(s.viewers) != 1 {
		t.Fatalf("закрыть A: A.closed=%d B.closed=%d viewers=%d", wins[0].closed, wins[1].closed, len(s.viewers))
	}

	// Новое окно A, потом запоздалый WindowClosing старого — не трогает новое.
	must(s.OpenViewerWindow("A", "a", "", 0, "dark"))
	oldA()
	if s.viewers["A"] == nil {
		t.Fatal("старое поколение закрыло новое окно A")
	}

	// Пользователь закрыл B крестиком: запись убрана, Close() сами не зовём.
	closers["B"]()
	if s.viewers["B"] != nil || wins[1].closed != 0 {
		t.Fatalf("крестик B: запись %v, closed %d", s.viewers["B"], wins[1].closed)
	}

	must(s.OpenViewerWindow("B", "b", "", 0, "dark"))
	must(s.CloseAllViewerWindows())
	if len(s.viewers) != 0 {
		t.Fatalf("после CloseAll viewers = %d", len(s.viewers))
	}
	if err := s.ViewerAction("A", "pin"); err == nil {
		t.Fatal("ViewerAction без окна — ожидалась ошибка")
	}
}
