package vektor

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// FailureType — тип инжектируемого сбоя.
type FailureType int

const (
	FailurePeerCrash      FailureType = iota // Пир перестаёт отвечать
	FailureNetPartition                      // Сетевой раздел (peer A ↔ peer B)
	FailureMessageCorrupt                    // Искажение содержимого сообщений
	FailureSlowResponse                      // Замедление ответов
	FailureIntermittent                      // Периодические отказы
)

// TriggerType — тип триггера для активации сбоя.
type TriggerType int

const (
	TriggerAfterCount TriggerType = iota // Срабатывает после N операций
	TriggerAfterTime                     // Срабатывает через T времени
	TriggerProbability                   // Срабатывает с вероятностью P
)

// FailureSpec — спецификация сбоя для инжектора.
type FailureSpec struct {
	Type     FailureType
	Trigger  TriggerType
	Count    int           // Для TriggerAfterCount: после скольких операций
	Duration time.Duration // Для TriggerAfterTime: через какое время; для FailureSlowResponse: задержка
	Prob     float64       // Для TriggerProbability: вероятность [0,1]
	Scope    string        // PeerID или "*" для глобального
}

// ChaosEngine — инжектор сбоев с детерминированным RNG.
type ChaosEngine struct {
	mu       sync.Mutex
	rng      *rand.Rand
	failures []activeFailure
	opCount  atomic.Int64
	startAt  time.Time
}

type activeFailure struct {
	spec    FailureSpec
	applied int64
}

// NewChaosEngine создаёт инжектор с фиксированным seed.
func NewChaosEngine(seed int64) *ChaosEngine {
	return &ChaosEngine{
		rng:     rand.New(rand.NewSource(seed)),
		startAt: time.Now(),
	}
}

// AddFailure регистрирует спецификацию сбоя.
func (e *ChaosEngine) AddFailure(spec FailureSpec) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures = append(e.failures, activeFailure{spec: spec})
}

// ClearFailures удаляет все зарегистрированные сбои.
func (e *ChaosEngine) ClearFailures() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures = nil
}

// Check проверяет нужно ли инжектировать сбой для данной операции.
// Возвращает: тип сбоя, задержку (для SlowResponse), и нужно ли блокировать операцию.
func (e *ChaosEngine) Check(peerID string) (failType FailureType, delay time.Duration, shouldFail bool) {
	opNum := e.opCount.Add(1)
	elapsed := time.Since(e.startAt)

	e.mu.Lock()
	defer e.mu.Unlock()

	for i := range e.failures {
		f := &e.failures[i]

		// Scope check
		if f.spec.Scope != "" && f.spec.Scope != "*" && f.spec.Scope != peerID {
			continue
		}

		triggered := false
		switch f.spec.Trigger {
		case TriggerAfterCount:
			triggered = int(opNum) > f.spec.Count
		case TriggerAfterTime:
			triggered = elapsed > f.spec.Duration
		case TriggerProbability:
			triggered = e.rng.Float64() < f.spec.Prob
		}

		if !triggered {
			continue
		}

		f.applied++

		switch f.spec.Type {
		case FailurePeerCrash:
			return FailurePeerCrash, 0, true
		case FailureNetPartition:
			return FailureNetPartition, 0, true
		case FailureMessageCorrupt:
			return FailureMessageCorrupt, 0, false // не блокируем, но данные искажены
		case FailureSlowResponse:
			return FailureSlowResponse, f.spec.Duration, false
		case FailureIntermittent:
			// 50% шанс отказа при каждом срабатывании
			if e.rng.Float64() < 0.5 {
				return FailureIntermittent, 0, true
			}
		}
	}

	return 0, 0, false
}

// Stats возвращает статистику инжектированных сбоев.
func (e *ChaosEngine) Stats() []FailureStat {
	e.mu.Lock()
	defer e.mu.Unlock()
	stats := make([]FailureStat, len(e.failures))
	for i, f := range e.failures {
		stats[i] = FailureStat{
			Type:    f.spec.Type,
			Trigger: f.spec.Trigger,
			Scope:   f.spec.Scope,
			Applied: f.applied,
		}
	}
	return stats
}

// FailureStat — статистика конкретного сбоя.
type FailureStat struct {
	Type    FailureType
	Trigger TriggerType
	Scope   string
	Applied int64
}

// String возвращает читаемое имя типа сбоя.
func (ft FailureType) String() string {
	switch ft {
	case FailurePeerCrash:
		return "PeerCrash"
	case FailureNetPartition:
		return "NetPartition"
	case FailureMessageCorrupt:
		return "MessageCorrupt"
	case FailureSlowResponse:
		return "SlowResponse"
	case FailureIntermittent:
		return "Intermittent"
	default:
		return fmt.Sprintf("Unknown(%d)", int(ft))
	}
}
