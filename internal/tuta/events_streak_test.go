package tuta

import (
	"testing"
	"time"
)

func TestReconnectStreakResetsAfterAHealthyConnection(t *testing.T) {
	var streak reconnectStreak
	tier := func(d time.Duration) int {
		switch {
		case d >= 5*time.Second && d <= 10*time.Second:
			return 1
		case d >= 20*time.Second && d <= 40*time.Second:
			return 2
		case d >= 60*time.Second && d <= 120*time.Second:
			return 3
		}
		return 0
	}
	if got := tier(streak.record(time.Second)); got != 1 {
		t.Fatalf("first quick drop: tier = %d, want 1", got)
	}
	if got := tier(streak.record(time.Second)); got != 2 {
		t.Fatalf("second quick drop: tier = %d, want 2", got)
	}
	if got := tier(streak.record(time.Second)); got != 3 {
		t.Fatalf("third quick drop: tier = %d, want 3", got)
	}
	if got := tier(streak.record(10 * time.Minute)); got != 1 {
		t.Fatalf("drop after a long-lived connection: tier = %d, want 1 (the streak must end, not accumulate for the process lifetime)", got)
	}
	if got := tier(streak.record(time.Second)); got != 2 {
		t.Fatalf("quick drop after the reset: tier = %d, want 2", got)
	}
}
