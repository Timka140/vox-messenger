// Package vektor реализует платформу «ВЕКТОР» (Верификатор Каналов Точности
// Распределённых систем) для контролируемого тестирования с эмуляцией сетевых аномалий.
package vektor

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// NetProfile — декларативное описание характеристик канала связи.
type NetProfile struct {
	LossRate    float64       // Вероятность потери пакета [0.0, 1.0]
	DelayBase   time.Duration // Базовая задержка (median)
	DelayJitter time.Duration // Случайное отклонение от базовой задержки (±jitter)
	ReorderProb float64       // Вероятность переупорядочивания [0.0, 1.0]
	CorruptProb float64       // Вероятность искажения данных [0.0, 1.0]
}

// Preset-профили типичных сетевых условий.
var (
	ProfileIdeal = NetProfile{}
	ProfileLAN   = NetProfile{DelayBase: 1 * time.Millisecond, DelayJitter: 500 * time.Microsecond}
	ProfileWiFi  = NetProfile{LossRate: 0.01, DelayBase: 15 * time.Millisecond, DelayJitter: 10 * time.Millisecond}
	Profile3G    = NetProfile{LossRate: 0.05, DelayBase: 100 * time.Millisecond, DelayJitter: 50 * time.Millisecond}
	Profile4G    = NetProfile{LossRate: 0.02, DelayBase: 40 * time.Millisecond, DelayJitter: 20 * time.Millisecond}
	ProfileBad   = NetProfile{LossRate: 0.15, DelayBase: 200 * time.Millisecond, DelayJitter: 100 * time.Millisecond, ReorderProb: 0.05}
	ProfileChaos = NetProfile{LossRate: 0.30, DelayBase: 300 * time.Millisecond, DelayJitter: 200 * time.Millisecond, ReorderProb: 0.10, CorruptProb: 0.02}
)

// ChannelModel эмулирует сетевой канал с задержками, потерями и джиттером.
// Потокобезопасен: профиль можно менять на лету.
type ChannelModel struct {
	mu      sync.RWMutex
	profile NetProfile
	rng     *rand.Rand

	// Статистика
	stats ChannelStats
}

// ChannelStats — счётчики событий канала.
type ChannelStats struct {
	Sent      int64
	Delivered int64
	Lost      int64
	Corrupted int64
	Reordered int64
	TotalDelay time.Duration
}

// NewChannel создаёт канал с заданным профилем и детерминированным RNG.
func NewChannel(profile NetProfile, seed int64) *ChannelModel {
	return &ChannelModel{
		profile: profile,
		rng:     rand.New(rand.NewSource(seed)),
	}
}

// SetProfile меняет профиль канала на лету (потокобезопасно).
func (c *ChannelModel) SetProfile(p NetProfile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profile = p
}

// Profile возвращает текущий профиль.
func (c *ChannelModel) Profile() NetProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.profile
}

// Stats возвращает текущую статистику.
func (c *ChannelModel) Stats() ChannelStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// ResetStats обнуляет статистику.
func (c *ChannelModel) ResetStats() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats = ChannelStats{}
}

// Transmit эмулирует передачу данных через канал.
// Возвращает: данные (возможно искажённые), задержку, ok=false если пакет потерян.
func (c *ChannelModel) Transmit(data []byte) (out []byte, delay time.Duration, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stats.Sent++

	// Потеря пакета
	if c.profile.LossRate > 0 && c.rng.Float64() < c.profile.LossRate {
		c.stats.Lost++
		return nil, 0, false
	}

	// Задержка = base ± jitter (нормальное распределение)
	delay = c.profile.DelayBase
	if c.profile.DelayJitter > 0 {
		jitter := time.Duration(c.rng.NormFloat64() * float64(c.profile.DelayJitter))
		delay += jitter
		if delay < 0 {
			delay = 0
		}
	}
	c.stats.TotalDelay += delay

	// Искажение данных
	out = make([]byte, len(data))
	copy(out, data)
	if c.profile.CorruptProb > 0 && c.rng.Float64() < c.profile.CorruptProb {
		if len(out) > 0 {
			pos := c.rng.Intn(len(out))
			out[pos] ^= byte(c.rng.Intn(255) + 1)
			c.stats.Corrupted++
		}
	}

	// Переупорядочивание: добавляем дополнительную задержку
	if c.profile.ReorderProb > 0 && c.rng.Float64() < c.profile.ReorderProb {
		delay += time.Duration(c.rng.Intn(100)) * time.Millisecond
		c.stats.Reordered++
	}

	c.stats.Delivered++
	return out, delay, true
}

// TransmitSync — блокирующая передача с реальным ожиданием задержки.
// Для тестов без виртуального времени.
func (c *ChannelModel) TransmitSync(data []byte) ([]byte, bool) {
	out, delay, ok := c.Transmit(data)
	if !ok {
		return nil, false
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return out, true
}

// DeliveryRate возвращает процент успешно доставленных пакетов.
func (c *ChannelModel) DeliveryRate() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stats.Sent == 0 {
		return 1.0
	}
	return float64(c.stats.Delivered) / float64(c.stats.Sent)
}

// AvgDelay возвращает среднюю задержку доставленных пакетов.
func (c *ChannelModel) AvgDelay() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stats.Delivered == 0 {
		return 0
	}
	return time.Duration(float64(c.stats.TotalDelay) / float64(c.stats.Delivered))
}

// JitterEstimate возвращает оценку джиттера (стд. отклонение задержки).
func (c *ChannelModel) JitterEstimate() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	j := c.profile.DelayJitter
	if j == 0 {
		return 0
	}
	// Для нормального распределения: ~68% укладываются в ±1σ
	return time.Duration(math.Abs(float64(j)))
}
