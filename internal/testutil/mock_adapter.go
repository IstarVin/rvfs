package testutil

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/IstarVin/rvfs/internal/remote"
)

// Call records a single method invocation on MockRemoteAdapter.
type Call struct {
	Method string
	Args   []interface{}
}

// MockRemoteAdapter implements remote.RemoteAdapter with configurable
// per-method behaviour and call recording for test verification.
//
// IMPORTANT: Callbacks (e.g. GetFunc) are invoked **outside** the mutex so
// that blocking callbacks do not deadlock concurrent mock calls.
type MockRemoteAdapter struct {
	mu    sync.Mutex
	calls []Call

	// Configurable return values. When a Func field is set it takes priority;
	// otherwise the corresponding Err / result field is returned.

	ListFunc  func(ctx context.Context, path string) ([]remote.FileInfo, error)
	ListItems []remote.FileInfo
	ListErr   error

	StatFunc   func(ctx context.Context, path string) (remote.FileInfo, error)
	StatResult remote.FileInfo
	StatErr    error

	GetFunc func(ctx context.Context, path string, dest io.Writer) error
	GetData []byte // written to dest on success
	GetErr  error

	GetRangeFunc func(ctx context.Context, path string, offset, length int64, dest io.Writer) error
	GetRangeErr  error

	PutFunc func(ctx context.Context, path string, src io.Reader, size int64, mtime time.Time) error
	PutErr  error

	DeleteFunc func(ctx context.Context, path string) error
	DeleteErr  error

	MkdirFunc func(ctx context.Context, path string) error
	MkdirErr  error

	RenameFunc func(ctx context.Context, src, dst string) error
	RenameErr  error

	ProbeFunc func(ctx context.Context) error
	ProbeErrs []error // scripted sequence; exhausted → nil
	probeIdx  int

	QuotaFunc   func(ctx context.Context) (remote.QuotaInfo, error)
	QuotaResult remote.QuotaInfo
	QuotaErr    error

	SupportsRangeVal bool
}

func (m *MockRemoteAdapter) record(method string, args ...interface{}) {
	m.calls = append(m.calls, Call{Method: method, Args: args})
}

// Calls returns a copy of all recorded calls.
func (m *MockRemoteAdapter) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// CallsFor returns all recorded calls matching the given method name.
func (m *MockRemoteAdapter) CallsFor(method string) []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Call
	for _, c := range m.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (m *MockRemoteAdapter) List(ctx context.Context, path string) ([]remote.FileInfo, error) {
	m.mu.Lock()
	m.record("List", path)
	fn := m.ListFunc
	items := m.ListItems
	fallbackErr := m.ListErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path)
	}
	return items, fallbackErr
}

func (m *MockRemoteAdapter) Stat(ctx context.Context, path string) (remote.FileInfo, error) {
	m.mu.Lock()
	m.record("Stat", path)
	fn := m.StatFunc
	result := m.StatResult
	fallbackErr := m.StatErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path)
	}
	return result, fallbackErr
}

func (m *MockRemoteAdapter) Get(ctx context.Context, path string, dest io.Writer) error {
	m.mu.Lock()
	m.record("Get", path)
	fn := m.GetFunc
	data := m.GetData
	fallbackErr := m.GetErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path, dest)
	}
	if data != nil {
		if _, writeErr := dest.Write(data); writeErr != nil {
			return writeErr
		}
	}
	return fallbackErr
}

func (m *MockRemoteAdapter) GetRange(ctx context.Context, path string, offset, length int64, dest io.Writer) error {
	m.mu.Lock()
	m.record("GetRange", path, offset, length)
	fn := m.GetRangeFunc
	fallbackErr := m.GetRangeErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path, offset, length, dest)
	}
	return fallbackErr
}

func (m *MockRemoteAdapter) Put(ctx context.Context, path string, src io.Reader, size int64, mtime time.Time) error {
	m.mu.Lock()
	m.record("Put", path, size)
	fn := m.PutFunc
	fallbackErr := m.PutErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path, src, size, mtime)
	}
	return fallbackErr
}

func (m *MockRemoteAdapter) Delete(ctx context.Context, path string) error {
	m.mu.Lock()
	m.record("Delete", path)
	fn := m.DeleteFunc
	fallbackErr := m.DeleteErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path)
	}
	return fallbackErr
}

func (m *MockRemoteAdapter) Mkdir(ctx context.Context, path string) error {
	m.mu.Lock()
	m.record("Mkdir", path)
	fn := m.MkdirFunc
	fallbackErr := m.MkdirErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, path)
	}
	return fallbackErr
}

func (m *MockRemoteAdapter) Rename(ctx context.Context, src, dst string) error {
	m.mu.Lock()
	m.record("Rename", src, dst)
	fn := m.RenameFunc
	fallbackErr := m.RenameErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, src, dst)
	}
	return fallbackErr
}

func (m *MockRemoteAdapter) Probe(ctx context.Context) error {
	m.mu.Lock()
	m.record("Probe")
	fn := m.ProbeFunc
	var scriptedErr error
	scriptedValid := false
	if fn == nil && m.probeIdx < len(m.ProbeErrs) {
		scriptedErr = m.ProbeErrs[m.probeIdx]
		scriptedValid = true
		m.probeIdx++
	}
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}
	if scriptedValid {
		return scriptedErr
	}
	return nil
}

func (m *MockRemoteAdapter) Quota(ctx context.Context) (remote.QuotaInfo, error) {
	m.mu.Lock()
	m.record("Quota")
	fn := m.QuotaFunc
	result := m.QuotaResult
	fallbackErr := m.QuotaErr
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}
	return result, fallbackErr
}

func (m *MockRemoteAdapter) SupportsRange() bool {
	return m.SupportsRangeVal
}

// Verify ensures the adapter was called at least once for the given method.
func (m *MockRemoteAdapter) Verify(method string) error {
	if len(m.CallsFor(method)) == 0 {
		return fmt.Errorf("expected at least one call to %s, got none", method)
	}
	return nil
}
