package state

import (
	"fmt"
	"testing"
	"time"
)

func TestPushRecentEventCapsAtCapAndDropsOldest(t *testing.T) {
	var buf []RecentEvent
	for i := 0; i < RecentEventsCap+5; i++ {
		buf = PushRecentEvent(buf, RecentEvent{
			At:    time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			Event: fmt.Sprintf("ev-%d", i),
		})
	}
	if got := len(buf); got != RecentEventsCap {
		t.Fatalf("len after overflow: got %d want %d", got, RecentEventsCap)
	}
	if got := buf[0].Event; got != "ev-5" {
		t.Fatalf("oldest entry after overflow: got %q want ev-5", got)
	}
	want := fmt.Sprintf("ev-%d", RecentEventsCap+4)
	if got := buf[len(buf)-1].Event; got != want {
		t.Fatalf("newest entry after overflow: got %q want %q", got, want)
	}
}
