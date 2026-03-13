package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client connects to a running rvfs mount's UNIX socket.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	scan *bufio.Scanner
}

// Dial connects to the IPC socket at sockPath with a 2-second timeout.
func Dial(sockPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("ipc: dial %s: %w", sockPath, err)
	}
	return &Client{
		conn: conn,
		enc:  json.NewEncoder(conn),
		scan: bufio.NewScanner(conn),
	}, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Status sends a "status" request and returns the parsed StatusResponse.
func (c *Client) Status() (StatusResponse, error) {
	if err := c.enc.Encode(Request{Cmd: "status"}); err != nil {
		return StatusResponse{}, fmt.Errorf("ipc: send status: %w", err)
	}
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return StatusResponse{}, fmt.Errorf("ipc: read status response: %w", err)
		}
		return StatusResponse{}, fmt.Errorf("ipc: connection closed")
	}
	raw := c.scan.Bytes()
	var errEnv struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &errEnv) == nil && errEnv.Error != "" {
		return StatusResponse{}, fmt.Errorf("ipc: status: %s", errEnv.Error)
	}
	var resp StatusResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return StatusResponse{}, fmt.Errorf("ipc: decode status response: %w", err)
	}
	return resp, nil
}

// Sync sends a "sync" request. If force is true, the server will clear retry
// timers before triggering the sync cycle.
func (c *Client) Sync(force bool) error {
	if err := c.enc.Encode(Request{Cmd: "sync", Force: force}); err != nil {
		return fmt.Errorf("ipc: send sync: %w", err)
	}
	raw, err := c.readRaw("sync")
	if err != nil {
		return err
	}
	var resp SyncResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("ipc: decode sync response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("sync failed: %s", resp.Err)
	}
	return nil
}

// Prefetch sends a "prefetch" request to start downloading path into cache.
func (c *Client) Prefetch(path string) error {
	if err := c.enc.Encode(Request{Cmd: "prefetch", Path: path}); err != nil {
		return fmt.Errorf("ipc: send prefetch: %w", err)
	}
	raw, err := c.readRaw("prefetch")
	if err != nil {
		return err
	}
	var resp ActionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("ipc: decode prefetch response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("prefetch failed: %s", resp.Err)
	}
	return nil
}

// Evict sends an "evict" request to remove path from local cache now.
func (c *Client) Evict(path string) error {
	if err := c.enc.Encode(Request{Cmd: "evict", Path: path}); err != nil {
		return fmt.Errorf("ipc: send evict: %w", err)
	}
	raw, err := c.readRaw("evict")
	if err != nil {
		return err
	}
	var resp ActionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("ipc: decode evict response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("evict failed: %s", resp.Err)
	}
	return nil
}

// Downloads sends a "downloads" request and returns current status entries.
// path=="" means list all active downloads.
func (c *Client) Downloads(path string) (DownloadStatusResponse, error) {
	if err := c.enc.Encode(Request{Cmd: "downloads", Path: path}); err != nil {
		return DownloadStatusResponse{}, fmt.Errorf("ipc: send downloads: %w", err)
	}
	raw, err := c.readRaw("downloads")
	if err != nil {
		return DownloadStatusResponse{}, err
	}
	var resp DownloadStatusResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return DownloadStatusResponse{}, fmt.Errorf("ipc: decode downloads response: %w", err)
	}
	return resp, nil
}

func (c *Client) readRaw(cmd string) ([]byte, error) {
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return nil, fmt.Errorf("ipc: read %s response: %w", cmd, err)
		}
		return nil, fmt.Errorf("ipc: connection closed")
	}
	raw := c.scan.Bytes()
	var errEnv struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &errEnv) == nil && errEnv.Error != "" {
		return nil, fmt.Errorf("ipc: %s: %s", cmd, errEnv.Error)
	}
	return raw, nil
}
