package services

import (
	"testing"
	"time"
)

func TestVoiceJoinRetryInterval(t *testing.T) {
	for a := 0; a < voiceJoinFastRetries; a++ {
		if got := voiceJoinRetryInterval(a); got != 3*time.Second {
			t.Errorf("попытка %d: %v, ждали 3s", a, got)
		}
	}
	for _, a := range []int{voiceJoinFastRetries, voiceJoinFastRetries + 1, 100} {
		if got := voiceJoinRetryInterval(a); got != 30*time.Second {
			t.Errorf("попытка %d: %v, ждали 30s", a, got)
		}
	}
}

func TestJoinWasKnown(t *testing.T) {
	cases := []struct {
		name              string
		seen, participant bool
		content           string
		want              bool
	}{
		{"первый join в чужой канал", false, false, "peer", false},
		{"повтор join в чужой канал", true, false, "peer", true},
		{"уже участник моего канала", false, true, "peer", true},
		{"ack", false, false, "ack", true},
	}
	for _, c := range cases {
		if got := joinWasKnown(c.seen, c.participant, c.content); got != c.want {
			t.Errorf("%s: %v, ждали %v", c.name, got, c.want)
		}
	}
}
