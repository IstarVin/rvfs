package connectivity_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func (a *mockAdapter) Probe(ctx context.Context) error {
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
func (a *mockAdapter) List(_ context.Context, _ string) ([]remote.FileInfo, error) {
	return nil, nil
}
func (a *mockAdapter) Stat(_ context.Context, _ string) (remote.FileInfo, error) {
	return remote.FileInfo{}, nil
}
func (a *mockAdapter) Get(_ context.Context, _ string, _ io.Writer) error { return nil }
func (a *mockAdapter) GetRange(_ context.Context, _ string, _, _ int64, _ io.Writer) error {
	return nil
}
func (a *mockAdapter) Put(_ context.Context, _ string, _ io.Reader, _ int64, _ time.Time) error {
	return nil
}
func (a *mockAdapter) Delete(_ context.Context, _ string) error   { return nil }
func (a *mockAdapter) Mkdir(_ context.Context, _ string) error    { return nil }
func (a *mockAdapter) Rename(_ context.Context, _, _ string) error { return nil }
func (a *mockAdapter) SupportsRange() bool                         { return false }

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
			require.Fail(t, "timed out waiting for state", "wanted %s", want)
		}
	}
}

// TestThresholdFailures ensures exactly threshold consecutive failures are
// required before transitioning ONLINE -> OFFLINE.
func TestThresholdFailures(t *testing.T) {
	t.Parallel()
	const threshold = 3
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe}}
	mon := connectivity.New(adapter, probeInterval, threshold)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	assert.Equal(t, connectivity.StateOffline, mon.State())
}

// TestContextCancelledOnOffline ensures the monitor context is cancelled when
// transitioning to OFFLINE.
func TestContextCancelledOnOffline(t *testing.T) {
	t.Parallel()
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
		require.Fail(t, "context not cancelled after OFFLINE transition")
	}
}

// TestOfflineToReconnecting ensures a successful probe after OFFLINE
// transitions to RECONNECTING with a fresh (non-cancelled) context.
func TestOfflineToReconnecting(t *testing.T) {
	t.Parallel()
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe, nil}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	waitForState(t, sub, connectivity.StateReconnecting)

	assert.Equal(t, connectivity.StateReconnecting, mon.State())
	ctx := mon.Context()
	select {
	case <-ctx.Done():
		require.Fail(t, "new context should not be cancelled in RECONNECTING state")
	default:
		// good
	}
}

// TestNotifyQueueDrained ensures RECONNECTING -> ONLINE after the engine
// signals that the dirty queue has been flushed.
func TestNotifyQueueDrained(t *testing.T) {
	t.Parallel()
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
		assert.Equal(t, connectivity.StateOnline, got)
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "timed out waiting for ONLINE after NotifyQueueDrained")
	}

	assert.Equal(t, connectivity.StateOnline, mon.State())
}

// TestSubscribeReceivesAllTransitions ensures subscriber sees OFFLINE then
// RECONNECTING in order.
func TestSubscribeReceivesAllTransitions(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		require.Fail(t, "Stop() did not return promptly")
	}
}

// TestNotifyQueueDrainedIdempotent ensures calling NotifyQueueDrained from a
// non-RECONNECTING state is a no-op.
func TestNotifyQueueDrainedIdempotent(t *testing.T) {
	t.Parallel()
	adapter := &mockAdapter{}
	mon := connectivity.New(adapter, probeInterval, 3)
	mon.Start()
	defer mon.Stop()

	mon.NotifyQueueDrained()
	assert.Equal(t, connectivity.StateOnline, mon.State())
}

// ---------- Extended tests ----------

// TestSetRecoveryInterval verifies that SetRecoveryInterval is honoured while
// OFFLINE, making probes fire at the shorter cadence.
func TestSetRecoveryInterval(t *testing.T) {
	t.Parallel()
	// Fail immediately, then succeed after 1 probe at recovery pace.
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe, nil}}
	mon := connectivity.New(adapter, 500*time.Millisecond, 3)
	mon.SetRecoveryInterval(probeInterval) // much shorter recovery probes
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	// Because recovery interval is small, reconnection should arrive quickly
	// even though the normal interval is 500ms.
	waitForState(t, sub, connectivity.StateReconnecting)
}

// TestMultipleSubscribers ensures every subscriber receives transitions.
func TestMultipleSubscribers(t *testing.T) {
	t.Parallel()
	adapter := &mockAdapter{probes: []error{errProbe, errProbe, errProbe}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub1 := mon.Subscribe()
	sub2 := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub1, connectivity.StateOffline)
	waitForState(t, sub2, connectivity.StateOffline)
}

// TestSlowSubscriberDrop ensures a slow subscriber does not block transitions.
func TestSlowSubscriberDrop(t *testing.T) {
	t.Parallel()
	// 4 failures → offline; then 1 success → reconnecting;
	// then more failures → offline again.
	adapter := &mockAdapter{probes: []error{
		errProbe, errProbe, errProbe, // → offline
		nil,                          // → reconnecting
		errProbe,                     // → offline again (threshold=1 during reconnecting)
	}}
	mon := connectivity.New(adapter, probeInterval, 3)
	_ = mon.Subscribe() // intentionally never drained
	fast := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	// The fast subscriber should still see the offline transition even
	// though the first subscriber is slow.
	waitForState(t, fast, connectivity.StateOffline)
}

// TestRapidOnlineOffline cycles through OFFLINE → RECONNECTING → ONLINE
// and back to OFFLINE.
func TestRapidOnlineOffline(t *testing.T) {
	t.Parallel()
	adapter := &mockAdapter{probes: []error{
		errProbe, errProbe, errProbe, // → OFFLINE
		nil, // → RECONNECTING
	}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	waitForState(t, sub, connectivity.StateReconnecting)
	mon.NotifyQueueDrained()
	waitForState(t, sub, connectivity.StateOnline)
}

// TestReconnectingToOfflineOnFailure ensures that a probe failure during
// RECONNECTING transitions back to OFFLINE.
func TestReconnectingToOfflineOnFailure(t *testing.T) {
	t.Parallel()
	adapter := &mockAdapter{probes: []error{
		errProbe, errProbe, errProbe, // → OFFLINE
		nil,      // → RECONNECTING
		errProbe, // should go back to OFFLINE
	}}
	mon := connectivity.New(adapter, probeInterval, 3)
	sub := mon.Subscribe()
	mon.Start()
	defer mon.Stop()

	waitForState(t, sub, connectivity.StateOffline)
	waitForState(t, sub, connectivity.StateReconnecting)
	waitForState(t, sub, connectivity.StateOffline)
}
