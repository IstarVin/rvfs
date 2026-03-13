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
	defer m.mu.Unlock()
	m.record("List", path)
	if m.ListFunc != nil {
		return m.ListFunc(ctx, path)
	}
	return m.ListItems, m.ListErr
}

func (m *MockRemoteAdapter) Stat(ctx context.Context, path string) (remote.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Stat", path)
	if m.StatFunc != nil {
		return m.StatFunc(ctx, path)
	}
	return m.StatResult, m.StatErr
}

func (m *MockRemoteAdapter) Get(ctx context.Context, path string, dest io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Get", path)
	if m.GetFunc != nil {
		return m.GetFunc(ctx, path, dest)
	}
	if m.GetData != nil {
		if _, err := dest.Write(m.GetData); err != nil {
			return err
		}
	}
	return m.GetErr
}

func (m *MockRemoteAdapter) GetRange(ctx context.Context, path string, offset, length int64, dest io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("GetRange", path, offset, length)
	if m.GetRangeFunc != nil {
		return m.GetRangeFunc(ctx, path, offset, length, dest)
	}
	return m.GetRangeErr
}

func (m *MockRemoteAdapter) Put(ctx context.Context, path string, src io.Reader, size int64, mtime time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Put", path, size)
	if m.PutFunc != nil {
		return m.PutFunc(ctx, path, src, size, mtime)
	}
	return m.PutErr
}

func (m *MockRemoteAdapter) Delete(ctx context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Delete", path)
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, path)
	}
	return m.DeleteErr
}

func (m *MockRemoteAdapter) Mkdir(ctx context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Mkdir", path)
	if m.MkdirFunc != nil {
		return m.MkdirFunc(ctx, path)
	}
	return m.MkdirErr
}

func (m *MockRemoteAdapter) Rename(ctx context.Context, src, dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Rename", src, dst)
	if m.RenameFunc != nil {
		return m.RenameFunc(ctx, src, dst)
	}
	return m.RenameErr
}

func (m *MockRemoteAdapter) Probe(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("Probe")
	if m.ProbeFunc != nil {
		return m.ProbeFunc(ctx)
	}
	if m.probeIdx < len(m.ProbeErrs) {
		err := m.ProbeErrs[m.probeIdx]
		m.probeIdx++
		return err
	}
	return nil
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
