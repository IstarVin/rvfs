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
	var resp StatusResponse
	if err := json.Unmarshal(c.scan.Bytes(), &resp); err != nil {
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
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return fmt.Errorf("ipc: read sync response: %w", err)
		}
		return fmt.Errorf("ipc: connection closed")
	}
	var resp SyncResponse
	if err := json.Unmarshal(c.scan.Bytes(), &resp); err != nil {
		return fmt.Errorf("ipc: decode sync response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("sync failed: %s", resp.Err)
	}
	return nil
}
