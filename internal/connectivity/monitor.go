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
	adapter          remote.RemoteAdapter
	interval         time.Duration
	recoveryInterval time.Duration // probe cadence while OFFLINE; 0 means use interval
	threshold        int           // consecutive failures required to declare OFFLINE

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

// SetRecoveryInterval sets a shorter probe interval used only while the
// monitor is in StateOffline. This lets the monitor detect when the network
// comes back more quickly than the normal online polling interval.
// Must be called before Start.
func (m *Monitor) SetRecoveryInterval(d time.Duration) {
	m.recoveryInterval = d
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

	timer := time.NewTimer(m.nextDelay())
	defer timer.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-timer.C:
			m.probe()
			timer.Reset(m.nextDelay())
		}
	}
}

// nextDelay returns how long to wait before the next probe.
// When offline and a recovery interval has been configured, that shorter
// interval is used so reconnection is detected quickly.
func (m *Monitor) nextDelay() time.Duration {
	m.mu.RLock()
	offline := m.state == StateOffline
	m.mu.RUnlock()
	if offline && m.recoveryInterval > 0 {
		return m.recoveryInterval
	}
	return m.interval
}

func (m *Monitor) probe() {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), m.interval)
	defer probeCancel()
	err := m.adapter.Probe(probeCtx)

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
