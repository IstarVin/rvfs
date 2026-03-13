package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// Handler is called by the Server for each incoming request.
// Implementations should return the appropriate response value (to be
// JSON-encoded and sent back) or an error string.
type Handler interface {
	// HandleStatus returns a StatusResponse.
	HandleStatus() (StatusResponse, error)
	// HandleSync triggers a sync cycle, clearing retry timers first if force==true.
	HandleSync(force bool) error
	// HandlePrefetch starts (or resumes) downloading path into the cache.
	HandlePrefetch(path string, sequential bool) error
	// HandleEvict removes path from the on-disk cache and marks it evicted.
	HandleEvict(path string) error
	// HandleDownloads returns download/cache status for path, or all active
	// downloads when path is empty.
	HandleDownloads(path string) (DownloadStatusResponse, error)
}

// Server listens on a UNIX socket and dispatches incoming requests to a Handler.
type Server struct {
	handler  Handler
	sockPath string
	ln       net.Listener
	wg       sync.WaitGroup
	closed   atomic.Bool
}

// NewServer creates a Server that will listen at sockPath and dispatch to handler.
func NewServer(sockPath string, handler Handler) *Server {
	return &Server{handler: handler, sockPath: sockPath}
}

// Listen starts accepting connections. It removes any stale socket file first.
func (s *Server) Listen() error {
	if err := ensureSockDir(s.sockPath); err != nil {
		return fmt.Errorf("ipc: create socket dir: %w", err)
	}
	// Remove stale socket from a previous run.
	_ = os.Remove(s.sockPath)

	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return fmt.Errorf("ipc: listen %s: %w", s.sockPath, err)
	}
	s.ln = ln

	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Close stops accepting new connections and waits for in-flight handlers to finish.
func (s *Server) Close() {
	if s.closed.Swap(true) {
		return
	}
	_ = s.ln.Close()
	s.wg.Wait()
	_ = os.Remove(s.sockPath)
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = enc.Encode(map[string]string{"error": "bad request"})
			return
		}

		switch req.Cmd {
		case "status":
			resp, err := s.handler.HandleStatus()
			if err != nil {
				_ = enc.Encode(map[string]string{"error": err.Error()})
			} else {
				_ = enc.Encode(resp)
			}
		case "sync":
			err := s.handler.HandleSync(req.Force)
			if err != nil {
				_ = enc.Encode(SyncResponse{OK: false, Err: err.Error()})
			} else {
				_ = enc.Encode(SyncResponse{OK: true})
			}
		case "prefetch":
			err := s.handler.HandlePrefetch(req.Path, req.Sequential)
			if err != nil {
				_ = enc.Encode(ActionResponse{OK: false, Err: err.Error()})
			} else {
				_ = enc.Encode(ActionResponse{OK: true})
			}
		case "evict":
			err := s.handler.HandleEvict(req.Path)
			if err != nil {
				_ = enc.Encode(ActionResponse{OK: false, Err: err.Error()})
			} else {
				_ = enc.Encode(ActionResponse{OK: true})
			}
		case "downloads":
			resp, err := s.handler.HandleDownloads(req.Path)
			if err != nil {
				_ = enc.Encode(map[string]string{"error": err.Error()})
			} else {
				_ = enc.Encode(resp)
			}
		default:
			_ = enc.Encode(map[string]string{"error": "unknown command"})
		}
	}
}
