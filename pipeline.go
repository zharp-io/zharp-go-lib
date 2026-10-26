package zharp

import (
	"sync"
	"time"
)

// maxQueued is how many logs wait to be sent. When the network is down for long, the oldest go first.
const maxQueued = 500

// pipeline is one running configuration's queue and the goroutine that sends it. Logging only
// adds to the queue; every request is made on that goroutine, never the caller's.
type pipeline struct {
	transport *transport
	interval  time.Duration
	maxBatch  int

	mu      sync.Mutex
	queue   []map[string]any
	urgent  bool
	closed  bool
	sending int
	wake    chan struct{}
	done    chan struct{}
}

func newPipeline(t *transport, interval time.Duration, maxBatch int) *pipeline {
	p := &pipeline{transport: t, interval: interval, maxBatch: maxBatch, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go p.run()
	return p
}

func (p *pipeline) push(entry map[string]any, urgent bool) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if len(p.queue) >= maxQueued {
		p.queue = p.queue[1:]
	}
	p.queue = append(p.queue, entry)
	if urgent {
		p.urgent = true
	}
	p.mu.Unlock()
	p.signal()
}

func (p *pipeline) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// flush sends everything queued and waits up to timeout for it; it reports whether all of it went.
func (p *pipeline) flush(timeout time.Duration) bool {
	p.mu.Lock()
	p.urgent = true
	p.mu.Unlock()
	p.signal()
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		empty := len(p.queue) == 0 && p.sending == 0
		p.mu.Unlock()
		if empty {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *pipeline) close(timeout time.Duration) bool {
	sent := p.flush(timeout)
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.signal()
	return sent
}

func (p *pipeline) run() {
	defer close(p.done)
	for {
		// Asleep until there's something to send.
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.mu.Unlock()
			<-p.wake
			p.mu.Lock()
		}
		if p.closed && len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		hurry := p.urgent || p.closed
		p.mu.Unlock()

		// Then wait out the interval, or less when something urgent arrives.
		if !hurry {
			timer := time.NewTimer(p.interval)
		waiting:
			for {
				select {
				case <-timer.C:
					break waiting
				case <-p.wake:
					p.mu.Lock()
					hurry = p.urgent || p.closed
					p.mu.Unlock()
					if hurry {
						timer.Stop()
						break waiting
					}
				}
			}
		}

		p.mu.Lock()
		n := len(p.queue)
		if n > p.maxBatch {
			n = p.maxBatch
		}
		batch := append([]map[string]any(nil), p.queue[:n]...)
		p.queue = p.queue[n:]
		if len(p.queue) == 0 {
			p.urgent = false
		}
		p.sending++
		p.mu.Unlock()

		func() {
			defer func() { _ = recover() }()
			p.transport.send(batch)
		}()

		p.mu.Lock()
		p.sending--
		p.mu.Unlock()
	}
}
