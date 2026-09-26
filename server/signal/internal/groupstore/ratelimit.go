package groupstore

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter — token bucket per (groupID, publicKey).
// Лимитирует частоту отправки сообщений в группу от одного пользователя.
// Idle-лимитеры собираются GC-подобной зачисткой чтобы не раздувать map.
type RateLimiter struct {
	burst  int
	refill rate.Limit

	mu       sync.Mutex
	buckets  map[string]*bucket
	idleTTL  time.Duration
	gcTicker *time.Ticker
}

type bucket struct {
	lim      *rate.Limiter
	lastUsed time.Time
}

// NewRateLimiter создаёт лимитер с заданным burst и скоростью пополнения (токенов/сек).
// idleTTL — через какое время бездействия bucket удаляется (освобождает память).
func NewRateLimiter(burst, refillPerSec int, idleTTL time.Duration) *RateLimiter {
	if burst <= 0 {
		burst = 30
	}
	if refillPerSec <= 0 {
		refillPerSec = 5
	}
	if idleTTL <= 0 {
		idleTTL = 10 * time.Minute
	}
	rl := &RateLimiter{
		burst:   burst,
		refill:  rate.Limit(refillPerSec),
		buckets: make(map[string]*bucket),
		idleTTL: idleTTL,
	}
	rl.gcTicker = time.NewTicker(idleTTL)
	go rl.gcLoop()
	return rl
}

// Allow возвращает true если пользователю разрешено отправить ещё одно сообщение в группу.
func (rl *RateLimiter) Allow(groupID, pubKey string) bool {
	key := groupID + "|" + pubKey
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rl.refill, rl.burst)}
		rl.buckets[key] = b
	}
	b.lastUsed = time.Now()
	rl.mu.Unlock()
	return b.lim.Allow()
}

func (rl *RateLimiter) gcLoop() {
	for range rl.gcTicker.C {
		rl.sweep()
	}
}

func (rl *RateLimiter) sweep() {
	cutoff := time.Now().Add(-rl.idleTTL)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if b.lastUsed.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}
