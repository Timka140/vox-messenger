package services

import (
	"encoding/base64"
	"runtime"
	"time"
)

// Живые превью экранов для диалога «Показать экран» (макет Dialogs, фаза 5b).
// Пока диалог открыт, раз в секунду снимаем кадр каждого дисплея шириной
// 320px и шлём фронту событием vox:screen-thumbs. Фронт не опрашивает —
// только событие (правило no-polling).
//
// Захват — BitBlt, не DXGI: DXGI Desktop Duplication отдаёт кадр только когда
// экран меняется, и для статичного экрана превью не приходило вовсе (замер
// 2026-09-15, два экрана 2560×1440: у неподвижного — ни одного кадра). BitBlt
// ~30 мс на экран раз в секунду — не нагрузка, и он не пересекается с DXGI
// стрима. Во время собственной демонстрации превью всё равно не снимаем.
const (
	screenThumbWidth    = 320
	screenThumbInterval = time.Second
	// screenThumbTTL — страховка: диалог закрыли, а StopScreenThumbs не пришёл.
	screenThumbTTL = 60 * time.Second
)

type screenThumb struct {
	Index  int    `json:"index"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	JPEG   string `json:"jpeg"` // base64
}

// StartScreenThumbs запускает превью (повторный вызов продлевает TTL).
func (s *ScreenShareService) StartScreenThumbs() error {
	s.thumbMu.Lock()
	defer s.thumbMu.Unlock()
	s.thumbUntil = time.Now().Add(screenThumbTTL)
	if s.thumbStop != nil {
		return nil
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.thumbStop, s.thumbDone = stop, done
	go s.thumbLoop(stop, done)
	return nil
}

// StopScreenThumbs останавливает превью и ждёт, пока захват закроется, —
// чтобы следующий StartScreenShare не открыл DXGI поверх ещё живого.
func (s *ScreenShareService) StopScreenThumbs() {
	s.thumbMu.Lock()
	stop, done := s.thumbStop, s.thumbDone
	s.thumbStop, s.thumbDone = nil, nil
	s.thumbMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (s *ScreenShareService) thumbLoop(stop, done chan struct{}) {
	defer close(done)
	runtime.LockOSThread() // DXGI/COM привязаны к потоку, как в captureLoop
	defer runtime.UnlockOSThread()

	displays := listScreenDisplays()
	caps := make([]screenCapture, len(displays))
	closeAll := func() {
		for i, c := range caps {
			if c != nil {
				c.Close()
				caps[i] = nil
			}
		}
	}
	defer closeAll()

	tick := time.NewTicker(screenThumbInterval)
	defer tick.Stop()
	for {
		s.mu.Lock()
		sharing := s.active
		s.mu.Unlock()
		if sharing {
			closeAll()
		} else {
			items := make([]screenThumb, 0, len(displays))
			for i, d := range displays {
				if caps[i] == nil {
					c, err := openScreenCapture(d.Index, screenThumbWidth)
					if err != nil {
						continue
					}
					caps[i] = c
				}
				f, err := caps[i].Capture()
				if err != nil {
					continue
				}
				jpg, err := encodeJPEG(f, 60)
				if err != nil {
					continue
				}
				items = append(items, screenThumb{Index: d.Index, Width: d.Width, Height: d.Height, JPEG: base64.StdEncoding.EncodeToString(jpg)})
			}
			s.network.emitEvent("vox:screen-thumbs", map[string]any{"items": items})
		}

		s.thumbMu.Lock()
		expired := time.Now().After(s.thumbUntil)
		if expired && s.thumbStop == stop {
			s.thumbStop, s.thumbDone = nil, nil
		}
		s.thumbMu.Unlock()
		if expired {
			return
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
