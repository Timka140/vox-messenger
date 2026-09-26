package vektor

import (
	"fmt"
	"testing"
	"time"
)

// === Этап 1: ChannelModel ===

func TestChannelIdeal(t *testing.T) {
	ch := NewChannel(ProfileIdeal, 42)

	for i := 0; i < 100; i++ {
		data := []byte(fmt.Sprintf("packet-%d", i))
		out, delay, ok := ch.Transmit(data)
		if !ok {
			t.Fatalf("ideal channel lost packet %d", i)
		}
		if delay != 0 {
			t.Errorf("ideal channel has delay: %v", delay)
		}
		if string(out) != string(data) {
			t.Errorf("ideal channel corrupted data")
		}
	}

	stats := ch.Stats()
	if stats.Sent != 100 || stats.Delivered != 100 || stats.Lost != 0 {
		t.Errorf("ideal stats: sent=%d delivered=%d lost=%d", stats.Sent, stats.Delivered, stats.Lost)
	}
	if ch.DeliveryRate() != 1.0 {
		t.Errorf("ideal delivery rate: %.2f", ch.DeliveryRate())
	}
}

func TestChannelWithLoss(t *testing.T) {
	ch := NewChannel(NetProfile{LossRate: 0.30}, 42)

	delivered := 0
	for i := 0; i < 1000; i++ {
		_, _, ok := ch.Transmit([]byte("data"))
		if ok {
			delivered++
		}
	}

	// С seed=42 и 30% потерь, ожидаем ~700 доставленных (±50)
	if delivered < 600 || delivered > 800 {
		t.Errorf("30%% loss: delivered %d/1000 (expected ~700)", delivered)
	}

	rate := ch.DeliveryRate()
	if rate < 0.60 || rate > 0.80 {
		t.Errorf("delivery rate %.2f outside expected range [0.60, 0.80]", rate)
	}
	t.Logf("30%% loss channel: delivered=%d/1000, rate=%.2f%%", delivered, rate*100)
}

func TestChannelWithDelay(t *testing.T) {
	ch := NewChannel(NetProfile{
		DelayBase:   50 * time.Millisecond,
		DelayJitter: 20 * time.Millisecond,
	}, 42)

	var totalDelay time.Duration
	for i := 0; i < 100; i++ {
		_, delay, ok := ch.Transmit([]byte("data"))
		if !ok {
			t.Fatal("unexpected loss")
		}
		totalDelay += delay
	}

	avgDelay := ch.AvgDelay()
	// Средняя задержка должна быть ~50ms (±20ms jitter)
	if avgDelay < 30*time.Millisecond || avgDelay > 70*time.Millisecond {
		t.Errorf("avg delay %v outside expected range [30ms, 70ms]", avgDelay)
	}
	t.Logf("delay channel: avg=%v, jitter_estimate=%v", avgDelay, ch.JitterEstimate())
}

func TestChannelDeterministic(t *testing.T) {
	// Два канала с одинаковым seed должны давать идентичные результаты
	ch1 := NewChannel(ProfileChaos, 12345)
	ch2 := NewChannel(ProfileChaos, 12345)

	for i := 0; i < 100; i++ {
		data := []byte(fmt.Sprintf("packet-%d", i))
		out1, delay1, ok1 := ch1.Transmit(data)
		out2, delay2, ok2 := ch2.Transmit(data)

		if ok1 != ok2 {
			t.Fatalf("determinism broken at packet %d: ok1=%v ok2=%v", i, ok1, ok2)
		}
		if delay1 != delay2 {
			t.Fatalf("determinism broken at packet %d: delay1=%v delay2=%v", i, delay1, delay2)
		}
		if ok1 && string(out1) != string(out2) {
			t.Fatalf("determinism broken at packet %d: data mismatch", i)
		}
	}

	s1 := ch1.Stats()
	s2 := ch2.Stats()
	if s1 != s2 {
		t.Errorf("stats mismatch: %+v vs %+v", s1, s2)
	}
}

func TestChannelProfileSwitch(t *testing.T) {
	ch := NewChannel(ProfileIdeal, 42)

	// Идеальный канал: 0 потерь
	for i := 0; i < 50; i++ {
		_, _, ok := ch.Transmit([]byte("data"))
		if !ok {
			t.Fatal("ideal channel lost packet")
		}
	}

	// Переключаем на Chaos
	ch.SetProfile(ProfileChaos)
	ch.ResetStats()

	lost := 0
	for i := 0; i < 200; i++ {
		_, _, ok := ch.Transmit([]byte("data"))
		if !ok {
			lost++
		}
	}

	if lost == 0 {
		t.Error("chaos profile should lose some packets")
	}
	t.Logf("profile switch: lost %d/200 after chaos", lost)
}

// === Этап 2: ChaosEngine ===

func TestChaosAfterCount(t *testing.T) {
	engine := NewChaosEngine(42)
	engine.AddFailure(FailureSpec{
		Type:    FailurePeerCrash,
		Trigger: TriggerAfterCount,
		Count:   5,
		Scope:   "*",
	})

	// Первые 5 операций — ОК
	for i := 0; i < 5; i++ {
		_, _, fail := engine.Check("peer1")
		if fail {
			t.Errorf("op %d should not fail", i+1)
		}
	}

	// 6-я операция — crash
	_, _, fail := engine.Check("peer1")
	if !fail {
		t.Error("op 6 should trigger PeerCrash")
	}
}

func TestChaosProbability(t *testing.T) {
	engine := NewChaosEngine(42)
	engine.AddFailure(FailureSpec{
		Type:    FailurePeerCrash,
		Trigger: TriggerProbability,
		Prob:    0.5,
		Scope:   "*",
	})

	failures := 0
	for i := 0; i < 100; i++ {
		_, _, fail := engine.Check("peer1")
		if fail {
			failures++
		}
	}

	// ~50% ± 15%
	if failures < 20 || failures > 80 {
		t.Errorf("probability trigger: %d/100 failures (expected ~50)", failures)
	}
	t.Logf("probability chaos: %d/100 failures", failures)
}

func TestChaosScope(t *testing.T) {
	engine := NewChaosEngine(42)
	engine.AddFailure(FailureSpec{
		Type:    FailurePeerCrash,
		Trigger: TriggerAfterCount,
		Count:   0, // always
		Scope:   "peer-A",
	})

	// peer-A fails
	_, _, fail := engine.Check("peer-A")
	if !fail {
		t.Error("peer-A should fail")
	}

	// peer-B is fine
	_, _, fail = engine.Check("peer-B")
	if fail {
		t.Error("peer-B should NOT fail (out of scope)")
	}
}

// === Этап 3: VirtualPeer + Scenario ===

func TestVirtualPeerDMWithLoss(t *testing.T) {
	alice, err := NewVirtualPeer(PeerConfig{
		Nickname:    "Alice",
		NetProfile:  NetProfile{LossRate: 0.20}, // 20% потеря
		ChannelSeed: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	bob, err := NewVirtualPeer(PeerConfig{
		Nickname:    "Bob",
		NetProfile:  ProfileIdeal,
		ChannelSeed: 43,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	// Устанавливаем связь
	alice.AddContact(bob.ID, "Bob")
	alice.ConnectTo(bob.ID)

	// Отправляем 50 сообщений
	sent := 0
	delivered := 0
	for i := 0; i < 50; i++ {
		_, err := alice.Messaging.SendMessage(bob.ID, fmt.Sprintf("msg-%d", i))
		sent++
		if err == nil {
			delivered++
		}
	}

	stats := alice.Channel().Stats()
	t.Logf("Alice→Bob: sent=%d, channel_delivered=%d, channel_lost=%d, rate=%.0f%%",
		stats.Sent, stats.Delivered, stats.Lost, alice.Channel().DeliveryRate()*100)

	// С 20% потерей ~40 доставлено, ~10 потеряно
	if stats.Lost == 0 {
		t.Error("expected some packet loss with 20% profile")
	}
	if stats.Delivered == 0 {
		t.Error("expected some packets delivered")
	}
}

func TestScenarioExecution(t *testing.T) {
	alice, _ := NewVirtualPeer(PeerConfig{Nickname: "Alice", NetProfile: ProfileIdeal, ChannelSeed: 1})
	defer alice.Close()

	bob, _ := NewVirtualPeer(PeerConfig{Nickname: "Bob", NetProfile: ProfileIdeal, ChannelSeed: 2})
	defer bob.Close()

	alice.AddContact(bob.ID, "Bob")
	alice.ConnectTo(bob.ID)

	scenario := &Scenario{
		Name:  "Basic DM Delivery",
		Peers: []*VirtualPeer{alice, bob},
		Steps: []Step{
			{Name: "Send 10 messages", Action: func() error {
				for i := 0; i < 10; i++ {
					if _, err := alice.Messaging.SendMessage(bob.ID, fmt.Sprintf("hello-%d", i)); err != nil {
						return err
					}
				}
				return nil
			}},
		},
		Checks: []Check{
			{Name: "All messages in DB", Verify: func() (bool, string) {
				var count int
				alice.Store.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
				if count == 10 {
					return true, fmt.Sprintf("%d messages", count)
				}
				return false, fmt.Sprintf("expected 10, got %d", count)
			}},
			{Name: "Channel delivery rate = 100%", Verify: func() (bool, string) {
				rate := alice.Channel().DeliveryRate()
				return rate == 1.0, fmt.Sprintf("%.0f%%", rate*100)
			}},
		},
	}

	result := scenario.Run()
	t.Log(result.Report())

	if !result.Passed {
		t.Error("scenario failed")
	}
}

func TestScenarioChaosRecovery(t *testing.T) {
	alice, _ := NewVirtualPeer(PeerConfig{Nickname: "Alice", NetProfile: ProfileIdeal, ChannelSeed: 1, ChaosSeed: 42})
	defer alice.Close()

	bob, _ := NewVirtualPeer(PeerConfig{Nickname: "Bob", NetProfile: ProfileIdeal, ChannelSeed: 2})
	defer bob.Close()

	alice.AddContact(bob.ID, "Bob")
	alice.ConnectTo(bob.ID)

	// Crash после 5 операций
	alice.Chaos().AddFailure(FailureSpec{
		Type:    FailurePeerCrash,
		Trigger: TriggerAfterCount,
		Count:   5,
		Scope:   "*",
	})

	scenario := &Scenario{
		Name:  "Chaos Recovery",
		Peers: []*VirtualPeer{alice, bob},
		Steps: []Step{
			{Name: "Send messages until crash", Action: func() error {
				for i := 0; i < 10; i++ {
					_, err := alice.Messaging.SendMessage(bob.ID, fmt.Sprintf("chaos-%d", i))
					_ = err // Some will fail due to chaos
				}
				return nil
			}},
			{Name: "Clear chaos and retry", Action: func() error {
				alice.Chaos().ClearFailures()
				for i := 10; i < 20; i++ {
					if _, err := alice.Messaging.SendMessage(bob.ID, fmt.Sprintf("recovery-%d", i)); err != nil {
						return err
					}
				}
				return nil
			}},
		},
		Checks: []Check{
			{Name: "Recovery messages delivered", Verify: func() (bool, string) {
				sent := alice.SentCountTo(bob.ID)
				return sent >= 10, fmt.Sprintf("sent %d to Bob", sent)
			}},
		},
	}

	result := scenario.Run()
	t.Log(result.Report())

	if !result.Passed {
		t.Error("chaos recovery scenario failed")
	}
}
