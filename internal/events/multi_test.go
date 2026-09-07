package events

import (
	"errors"
	"testing"
	"time"
)

func TestMulti_FansOutEveryEvent(t *testing.T) {
	a, b := &Recorder{}, &Recorder{}
	var m Observer = Multi{a, b, Nop{}}

	m.TargetHealthChanged("SPL", "One", false, "down")
	m.TargetTainted("SPL", "One", "500", time.Second)
	m.RequestRerouted("SPL", "One", "500")
	m.NoHealthyTargets("SPL", 2)
	m.UpstreamRequest("SPL", "One", "eth_call", 200, time.Millisecond, errors.New("x"))

	for name, r := range map[string]*Recorder{"first": a, "second": b} {
		got := r.All()
		if len(got) != 5 {
			t.Fatalf("%s observer got %d events, want 5: %+v", name, len(got), got)
		}
		kinds := []string{KindHealthChanged, KindTainted, KindRerouted, KindNoHealthy, KindUpstreamRequest}
		for i, k := range kinds {
			if got[i].Kind != k || got[i].Chain != "SPL" {
				t.Errorf("%s observer event %d = %+v, want kind %s", name, i, got[i], k)
			}
		}
		if got[3].Attempted != 2 || got[4].Status != 200 || got[4].Err == nil {
			t.Errorf("%s observer lost event details: %+v", name, got)
		}
	}
}

func TestMulti_EmptyIsSafe(t *testing.T) {
	var m Observer = Multi{}
	m.RequestRerouted("SPL", "One", "x") // must not panic
}
