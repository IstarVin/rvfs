package connectivity

import (
	"context"
	"sync"
	"time"

	"github.com/IstarVin/rvfs/internal/remote"
)

// ConnState is the connectivity state of the mount.
type ConnState int

const (
	StateOnline       ConnState = iota // connected and syncing normally
	StateOffline                       // connection lost
	StateReconnecting                  // connection restored; dirty queue draining
)

func (s ConnState) String() string {
	switch s {
	case StateOnline:
		return "ONLINE"
	case StateOffline:
		return "OFFLINE"
	case StateReconnecting:
		return "RECONNECTING"
	default:
		return "UNKNOWN"
	}
}

// Monitor probes the remote adapter on a configurable ticker and drives a
// 3-state connectivity machine:
//
// ONLINE -> (N consecutive failures) -> OFFLINE
// OFFLINE -> (probe succeeds)        -> RECONNECTING
// RECONNECTING -> (NotifyQueueDrained called by sync engine) -> ONLINE
//
// The context returned by Context() is cancelled for the duration of OFFLINE
// and renewed when transitioning to RECONNECTING.
type Monitor struct {
	adapter   remote.RemoteAdapter
	interval  time.Duration
	threshold int // consecutive failures required to declare OFFLINE

	mu          sync.RWMutex
	state       ConnState
	failures    int
	subscribers []chan ConnState
	ctx         context.Context
	cancel      context.CancelFunc

	stopCh chan struct{}
	doneCh chan struct{}
}

// New creates a Monitor. Call Start() to begin the probe loop.
// threshold is the number of consecutive probe failures before declaring OFFLINE.
func New(adapter remote.RemoteAdapter, interval time.Duration, threshold int) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		adapter:   adapter,
		interval:  interval,
		threshold: threshold,
		state:     StateOnline,
		ctx:       ctx,
		cancel:    cancel,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start launches the background probe loop. Call Stop() to shut it down.
func (m *Monitor) Start() {
	go m.loop()
}

// Stop signals the probe loop to stop and waits for it to exit.
func (m *Monitor) Stop() {
	close(m.stopCh)
	<-m.doneCh
}

// State returns the current connectivity state.
func (m *Monitor) State() ConnState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Subscribe returns a channel that receives a ConnState value on every state
// transition. The channel is buffered (cap 1); if the subscriber is slow,
// intermediate transitions may be dropped rather than blocking the monitor.
func (m *Monitor) Subscribe() <-chan ConnState {
	ch := make(chan ConnState, 1)
	m.mu.Lock()
	m.subscribers = append(m.subscribers, ch)
	m.mu.Unlock()
	return ch
}

// Context returns a context.Context that is valid while ONLINE or
// RECONNECTING, and cancelled for the entire OFFLINE period. A fresh context
// is created on each OFFLINE -> RECONNECTING transition.
func (m *Monitor) Context() context.Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ctx
}

// NotifyQueueDrained is called by the sync engine once the dirty queue has
// been fully flushed after a reconnect. It transitions the state from
// RECONNECTING to ONLINE and notifies all subscribers.
func (m *Monitor) NotifyQueueDrained() {
	m.mu.Lock()
	if m.state != StateReconnecting {
		m.mu.Unlock()
		return
	}
	m.state = StateOnline
	subs := m.copySubscribers()
	m.mu.Unlock()
	m.broadcast(subs, StateOnline)
}

// ---------- probe loop ----------

func (m *Monitor) loop() {
	defer close(m.doneCh)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.probe()
		}
	}
}

func (m *Monitor) probe() {
	err := m.adapter.Probe()

	m.mu.Lock()
	if err != nil {
		m.failures++
		if m.state == StateOnline && m.failures >= m.threshold {
			m.state = StateOffline
			m.cancel()
			subs := m.copySubscribers()
			m.mu.Unlock()
			m.broadcast(subs, StateOffline)
			return
		}
		if m.state == StateReconnecting {
			// Lost connection again while draining - go back offline.
			m.state = StateOffline
			m.cancel()
			subs := m.copySubscribers()
			m.mu.Unlock()
			m.broadcast(subs, StateOffline)
			return
		}
		m.mu.Unlock()
		return
	}

	// Probe succeeded.
	m.failures = 0
	if m.state == StateOffline {
		// Renew the context for the RECONNECTING / ONLINE phase.
		ctx, cancel := context.WithCancel(context.Background())
		m.ctx = ctx
		m.cancel = cancel
		m.state = StateReconnecting
		subs := m.copySubscribers()
		m.mu.Unlock()
		m.broadcast(subs, StateReconnecting)
		return
	}
	m.mu.Unlock()
}

// copySubscribers returns a snapshot of the current subscriber list.
// Must be called with m.mu held.
func (m *Monitor) copySubscribers() []chan ConnState {
	snap := make([]chan ConnState, len(m.subscribers))
	copy(snap, m.subscribers)
	return snap
}

func (m *Monitor) broadcast(subs []chan ConnState, s ConnState) {
	for _, ch := range subs {
		select {
		case ch <- s:
		default:
			// Drop if subscriber is not consuming fast enough.
		}
	}
}
