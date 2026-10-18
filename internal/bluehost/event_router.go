package bluehost

import "sync"

// NewShowEventRouter drains reentrant scene emissions without recursive calls.
// One cascade is bounded; exceeding it reports an error rather than wedging Orion.
func NewShowEventRouter(deliver ShowEmitSink, onOverflow func()) ShowEmitSink {
	type event struct {
		topic   string
		payload any
	}
	var mu sync.Mutex
	var queue []event
	draining := false
	return func(topic string, payload any) {
		mu.Lock()
		if len(queue) >= 1024 {
			mu.Unlock()
			if onOverflow != nil {
				onOverflow()
			}
			return
		}
		queue = append(queue, event{topic, payload})
		if draining {
			mu.Unlock()
			return
		}
		draining = true
		for count := 0; len(queue) > 0; count++ {
			if count >= 1024 {
				queue = nil
				draining = false
				mu.Unlock()
				if onOverflow != nil {
					onOverflow()
				}
				return
			}
			next := queue[0]
			queue = queue[1:]
			mu.Unlock()
			deliver(next.topic, next.payload)
			mu.Lock()
		}
		queue = nil
		draining = false
		mu.Unlock()
	}
}
