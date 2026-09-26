package vektor

import (
	"fmt"
	"time"
)

// ScenarioResult — результат выполнения сценария.
type ScenarioResult struct {
	Name     string
	Passed   bool
	Duration time.Duration
	Checks   []CheckResult
	Metrics  map[string]float64
}

// CheckResult — результат одной проверки.
type CheckResult struct {
	Name   string
	Passed bool
	Detail string
}

// Scenario — описание тестового сценария.
type Scenario struct {
	Name    string
	Peers   []*VirtualPeer
	Steps   []Step
	Checks  []Check
}

// Step — один шаг сценария.
type Step struct {
	Name   string
	Action func() error
}

// Check — проверка после выполнения сценария.
type Check struct {
	Name   string
	Verify func() (bool, string) // ok, detail
}

// Run выполняет сценарий и собирает результаты.
func (s *Scenario) Run() ScenarioResult {
	start := time.Now()
	result := ScenarioResult{
		Name:    s.Name,
		Passed:  true,
		Metrics: make(map[string]float64),
	}

	// Выполняем шаги
	for i, step := range s.Steps {
		if err := step.Action(); err != nil {
			result.Checks = append(result.Checks, CheckResult{
				Name:   fmt.Sprintf("Step %d: %s", i+1, step.Name),
				Passed: false,
				Detail: err.Error(),
			})
			result.Passed = false
			result.Duration = time.Since(start)
			return result
		}
	}

	// Выполняем проверки
	for _, check := range s.Checks {
		ok, detail := check.Verify()
		result.Checks = append(result.Checks, CheckResult{
			Name:   check.Name,
			Passed: ok,
			Detail: detail,
		})
		if !ok {
			result.Passed = false
		}
	}

	// Собираем метрики каналов
	for i, peer := range s.Peers {
		stats := peer.Channel().Stats()
		prefix := fmt.Sprintf("peer%d", i)
		result.Metrics[prefix+"_sent"] = float64(stats.Sent)
		result.Metrics[prefix+"_delivered"] = float64(stats.Delivered)
		result.Metrics[prefix+"_lost"] = float64(stats.Lost)
		result.Metrics[prefix+"_corrupted"] = float64(stats.Corrupted)
		result.Metrics[prefix+"_delivery_rate"] = peer.Channel().DeliveryRate()
		if stats.Delivered > 0 {
			result.Metrics[prefix+"_avg_delay_ms"] = float64(peer.Channel().AvgDelay()) / float64(time.Millisecond)
		}

		// Chaos stats
		for j, cs := range peer.Chaos().Stats() {
			key := fmt.Sprintf("%s_chaos%d_%s", prefix, j, cs.Type)
			result.Metrics[key] = float64(cs.Applied)
		}
	}

	result.Duration = time.Since(start)
	return result
}

// Report генерирует текстовый отчёт.
func (r *ScenarioResult) Report() string {
	status := "✅ PASS"
	if !r.Passed {
		status = "❌ FAIL"
	}

	report := fmt.Sprintf("=== %s [%s] (%v) ===\n", r.Name, status, r.Duration.Round(time.Millisecond))

	for _, c := range r.Checks {
		mark := "✓"
		if !c.Passed {
			mark = "✗"
		}
		report += fmt.Sprintf("  %s %s", mark, c.Name)
		if c.Detail != "" {
			report += fmt.Sprintf(" — %s", c.Detail)
		}
		report += "\n"
	}

	if len(r.Metrics) > 0 {
		report += "  Metrics:\n"
		for k, v := range r.Metrics {
			report += fmt.Sprintf("    %s = %.2f\n", k, v)
		}
	}

	return report
}
