package connectivity_test

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/remote"
)

// mockAdapter implements remote.RemoteAdapter with a scripted Probe sequence.
// An empty results slice means Probe always returns nil (success).
type mockAdapter struct {
	mu     sync.Mutex
	probes []error
	idx    int
}

func (a *mockAdapter) Probe() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.idx >= len(a.probes) {
		return nil
	}
	err := a.probes[a.idx]
	a.idx++
	return err
}

// Remaining RemoteAdapter methods - not used by Monitor.
func (a *mockAdapter) List(_ string) ([]remote.FileInfo, error)              { return nil, nil }
func (a *mockAdapter) Stat(_ string) (remote.FileInfo, error)                { return remote.FileInfo{}, nil }
func (a *mockAdapter) Get(_ string, _ io.Writer) error                       { return nil }
func (a *mockAdapter) GetRange(_ string, _, _ int64, _ io.Writer) error      { return nil }
func (a *mockAdapter) Put(_ string, _ io.Reader, _ int64, _ time.Time) error { return nil }
func (a *mockAdapter) Delete(_ string) error                                 { return nil }
func (a *mockAdapter) Mkdir(_ string) error                                  { return nil }
func (a *mockAdapter) Rename(_, _ string) error                              { return nil }
func (a *mockAdapter) SupportsRange() bool                                   { return false }

var errProbe = errors.New("probe: connection refused")

// probeInterval is short so tests run fast.
const probeInterval = 10 * time.Millisecond

// waitForState waits up to 2 s for the subscriber channel to emit the expected
// state.
func waitForState(t *testing.T, sub <-chan connectivity.ConnState, want connectivity.ConnState) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-sub:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %s", want)
		}
	}
}

// TestThresholdFailures ensures exactly threshold consecutive failures are
// required before transitioning ONLINE -> OFFLINE.
func TestThresholdFailures(t *testing.T) {
	const threshold = 3
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe}}
	mon := connectivity.New(adapter, probeInterval, threshold)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)

	if got := mon.State(); got != connectivity.StateOffline {
		t.Fatalf("expected OFFLINE after threshold, got %s", got)
	}
}

// TestContextCancelledOnOffline ensures the monitor context is cancelled when
// transitioning to OFFLINE.
func TestContextCancelledOnOffline(t *testing.T) {
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe}}
	mon := connectivity.New(adapter, probeInterval, 3)
	ctx := mon.Context()
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)

	select {
	case <-ctx.Done():
	// good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("context not cancelled after OFFLINE transition")
	}
}

// TestOfflineToReconnecting ensures a successful probe after OFFLINE
// transitions to RECONNECTING with a fresh (non-cancelled) context.
func TestOfflineToReconnecting(t *testing.T) {
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe, nil}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	waitForState(t, sub, connectivity.StateReconnecting)

	if got := mon.State(); got != connectivity.StateReconnecting {
		t.Fatalf("expected RECONNECTING, got %s", got)
	}
	ctx := mon.Context()
	select {
	case <-ctx.Done():
		t.Fatal("new context should not be cancelled in RECONNECTING state")
	default:
		// good
	}
}

// TestNotifyQueueDrained ensures RECONNECTING -> ONLINE after the engine
// signals that the dirty queue has been flushed.
func TestNotifyQueueDrained(t *testing.T) {
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe, nil}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	waitForState(t, sub, connectivity.StateReconnecting)

	mon.NotifyQueueDrained()

	select {
	case got := <-sub:
		if got != connectivity.StateOnline {
			t.Fatalf("expected ONLINE after NotifyQueueDrained, got %s", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for ONLINE after NotifyQueueDrained")
	}

	if got := mon.State(); got != connectivity.StateOnline {
		t.Fatalf("State() should be ONLINE, got %s", got)
	}
}

// TestSubscribeReceivesAllTransitions ensures subscriber sees OFFLINE then
// RECONNECTING in order.
func TestSubscribeReceivesAllTransitions(t *testing.T) {
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe, nil}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	for _, want := range []connectivity.ConnState{connectivity.StateOffline, connectivity.StateReconnecting} {
		waitForState(t, sub, want)
	}
}

// TestStopNoGoroutineLeak ensures Stop() returns promptly.
func TestStopNoGoroutineLeak(t *testing.T) {
	adapter := &mockAdapter{}
	mon := connectivity.New(adapter, probeInterval, 3)
	mon.Start()

	done := make(chan struct{})
	go func() {
		mon.Stop()
		close(done)
	}()

	select {
	case <-done:
	// good
	case <-time.After(1 * time.Second):
		t.Fatal("Stop() did not return promptly")
	}
}

// TestNotifyQueueDrainedIdempotent ensures calling NotifyQueueDrained from a
// non-RECONNECTING state is a no-op.
func TestNotifyQueueDrainedIdempotent(t *testing.T) {
	adapter := &mockAdapter{}
	mon := connectivity.New(adapter, probeInterval, 3)
	mon.Start()
	defer mon.Stop()

	mon.NotifyQueueDrained()
	if got := mon.State(); got != connectivity.StateOnline {
		t.Fatalf("expected ONLINE after spurious NotifyQueueDrained, got %s", got)
	}
}
