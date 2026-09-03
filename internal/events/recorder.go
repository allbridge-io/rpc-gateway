package events

import (
	"sync"
	"time"
)

// Event kinds recorded by Recorder.
const (
	KindHealthChanged   = "health_changed"
	KindTainted         = "tainted"
	KindRerouted        = "rerouted"
	KindNoHealthy       = "no_healthy_targets"
	KindUpstreamRequest = "upstream_request"
)

// Event is one recorded observer call.
type Event struct {
	Kind      string
	Chain     string
	Target    string
	Reason    string
	Method    string
	Healthy   bool
	Status    int
	Duration  time.Duration
	Err       error
	Attempted int
}

// Recorder is an Observer that keeps every event in memory. Tests use it to
// assert what the core reported; it also serves as the reference for any
// exporter (log, OTLP) of what must be emitted.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *Recorder) add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *Recorder) TargetHealthChanged(chain, target string, healthy bool, reason string) {
	r.add(Event{Kind: KindHealthChanged, Chain: chain, Target: target, Healthy: healthy, Reason: reason})
}

func (r *Recorder) TargetTainted(chain, target, reason string, duration time.Duration) {
	r.add(Event{Kind: KindTainted, Chain: chain, Target: target, Reason: reason, Duration: duration})
}

func (r *Recorder) RequestRerouted(chain, target, reason string) {
	r.add(Event{Kind: KindRerouted, Chain: chain, Target: target, Reason: reason})
}

func (r *Recorder) NoHealthyTargets(chain string, attempted int) {
	r.add(Event{Kind: KindNoHealthy, Chain: chain, Attempted: attempted})
}

func (r *Recorder) UpstreamRequest(chain, target, method string, status int, duration time.Duration, err error) {
	r.add(Event{Kind: KindUpstreamRequest, Chain: chain, Target: target, Method: method, Status: status, Duration: duration, Err: err})
}

// All returns a copy of every recorded event.
func (r *Recorder) All() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Of returns the events of one kind, optionally filtered by target ("" = any).
func (r *Recorder) Of(kind, target string) []Event {
	var out []Event
	for _, e := range r.All() {
		if e.Kind == kind && (target == "" || e.Target == target) {
			out = append(out, e)
		}
	}
	return out
}

// Reset forgets recorded events.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}
