package audio

import (
	"sync"
	"time"
)

// DeviceChange сообщает о смене default-устройства одного из типов.
// NewName — имя устройства, ставшего default'ом (для лога).
type DeviceChange struct {
	Type    DeviceType
	NewName string
}

// DefaultDeviceWatcher периодически опрашивает ListDevices и шлёт DeviceChange
// в канал, когда default capture или default playback меняется (юзер
// переключил наушники/колонки в системе).
//
// Реализация — polling, а не нативный IMMNotificationClient/AudioObject. Минус:
// задержка реакции до интервала опроса (default 2s). Плюс: кросс-платформенно
// без CGO COM-кода и связанных threading-граблей. Для UX переключения
// устройств 2 сек приемлемо.
//
// Использование:
//
//	w := audio.NewDefaultDeviceWatcher(2 * time.Second)
//	go func() {
//	    for ev := range w.Events() {
//	        // restart capture/playback
//	    }
//	}()
//	defer w.Stop()
type DefaultDeviceWatcher struct {
	interval time.Duration
	events   chan DeviceChange
	done     chan struct{}
	stopOnce sync.Once
}

// NewDefaultDeviceWatcher запускает goroutine опроса в фоне. Канал Events()
// получает DeviceChange по факту смены — initial state НЕ эмитит.
func NewDefaultDeviceWatcher(interval time.Duration) *DefaultDeviceWatcher {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	w := &DefaultDeviceWatcher{
		interval: interval,
		events:   make(chan DeviceChange, 4),
		done:     make(chan struct{}),
	}
	go w.loop()
	return w
}

// Events — канал, в который шлются обнаруженные смены default-устройств.
// Закрывается при Stop().
func (w *DefaultDeviceWatcher) Events() <-chan DeviceChange {
	return w.events
}

// Stop сигнализирует goroutine прекратить опрос и закрывает канал Events.
// Идемпотентно — повторные вызовы no-op.
func (w *DefaultDeviceWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.done)
	})
}

func (w *DefaultDeviceWatcher) loop() {
	defer close(w.events)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	prevCap := defaultDeviceName(DeviceCapture)
	prevPb := defaultDeviceName(DevicePlayback)

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			curCap := defaultDeviceName(DeviceCapture)
			curPb := defaultDeviceName(DevicePlayback)

			if curCap != "" && curCap != prevCap {
				prevCap = curCap
				w.send(DeviceChange{Type: DeviceCapture, NewName: curCap})
			}
			if curPb != "" && curPb != prevPb {
				prevPb = curPb
				w.send(DeviceChange{Type: DevicePlayback, NewName: curPb})
			}
		}
	}
}

func (w *DefaultDeviceWatcher) send(ev DeviceChange) {
	select {
	case w.events <- ev:
	default:
		// Если потребитель отстаёт — дропаем event. На практике рестарт
		// устройства занимает < 200ms, очередь из 4 событий хватит даже при
		// быстром переключении мульти-устройств.
	}
}

// defaultDeviceName возвращает имя default-устройства указанного типа.
// Пустая строка если default не определён или enumeration упала.
func defaultDeviceName(dt DeviceType) string {
	for _, d := range ListDevices(dt) {
		if d.IsDefault {
			return d.Name
		}
	}
	return ""
}
