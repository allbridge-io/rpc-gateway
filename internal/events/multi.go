package events

import "time"

// Multi fans every event out to several observers in order, for example the
// structured log and an OTLP exporter, or a log and a test recorder.
type Multi []Observer

func (m Multi) TargetHealthChanged(chain, target string, healthy bool, reason string) {
	for _, o := range m {
		o.TargetHealthChanged(chain, target, healthy, reason)
	}
}

func (m Multi) TargetTainted(chain, target, reason string, duration time.Duration) {
	for _, o := range m {
		o.TargetTainted(chain, target, reason, duration)
	}
}

func (m Multi) RequestRerouted(chain, target, reason string) {
	for _, o := range m {
		o.RequestRerouted(chain, target, reason)
	}
}

func (m Multi) NoHealthyTargets(chain string, attempted int) {
	for _, o := range m {
		o.NoHealthyTargets(chain, attempted)
	}
}

func (m Multi) UpstreamRequest(chain, target, method string, status int, duration time.Duration, err error) {
	for _, o := range m {
		o.UpstreamRequest(chain, target, method, status, duration, err)
	}
}
