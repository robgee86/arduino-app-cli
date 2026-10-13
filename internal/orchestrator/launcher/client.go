// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Package launcher is the client of the app launcher control socket: one JSON request per
// connection, answered by one JSON line.
package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// SocketName is the control socket file name inside the runtime directory.
const SocketName = "app-launcher.sock"

const maxResponseBytes = 1024 * 1024

// Error codes of the launcher protocol.
const (
	CodeBadRequest  = "bad_request"
	CodeNotFound    = "not_found"
	CodeUnsupported = "unsupported"
	CodeBusy        = "busy"
	CodeInternal    = "internal"
	// CodePrepareFailed is a start whose dependencies failed to install: the message ends with the install output.
	CodePrepareFailed = "prepare_failed"
)

// Error is a request the launcher refused or failed.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("app launcher: %s: %s", e.Code, e.Message)
}

type Timings struct {
	StopMs    float64 `json:"stop_ms"`
	PrepareMs float64 `json:"prepare_ms"`
	ForkMs    float64 `json:"fork_ms"`
}

type StartResponse struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Warm      bool      `json:"warm"`
	Runner    string    `json:"runner"`
	// Prepared tells the launcher installed the app dependencies before the start.
	Prepared bool `json:"prepared"`
	// Ports maps each public port the launcher forwards to the internal port of the app.
	Ports   map[int]int `json:"ports"`
	Timings Timings     `json:"timings"`
}

type StoppedChild struct {
	App      string `json:"app"`
	PID      int    `json:"pid"`
	ExitCode *int   `json:"exit_code"`
	Signal   *int   `json:"signal"`
}

type StopResponse struct {
	Stopped []StoppedChild `json:"stopped"`
}

// Child states reported by the launcher.
const (
	StateRunning  = "running"
	StateStopping = "stopping"
)

type Child struct {
	App       string    `json:"app"`
	PID       int       `json:"pid"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	// Ports maps each public port the launcher forwards to the internal port of the app.
	Ports  map[int]int `json:"ports"`
	Runner string      `json:"runner"`
	Warm   bool        `json:"warm"`
}

type StatusResponse struct {
	Children []Child        `json:"children"`
	Preload  map[string]any `json:"preload"`
	Version  string         `json:"version"`
	UptimeS  float64        `json:"uptime_s"`
}

// Client talks to the launcher on a Unix socket. Each call bounds the whole exchange by
// timeout, or by the context deadline when it is earlier.
type Client struct {
	socket  string
	timeout time.Duration
}

func New(socket string, timeout time.Duration) *Client {
	return &Client{socket: socket, timeout: timeout}
}

// WithTimeout returns a client on the same socket with another timeout.
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	return &Client{socket: c.socket, timeout: timeout}
}

func (c *Client) Socket() string {
	return c.socket
}

// Commands of the launcher protocol.
const (
	cmdPing   = "ping"
	cmdStart  = "start"
	cmdStop   = "stop"
	cmdStatus = "status"
)

type request struct {
	Cmd  string            `json:"cmd"`
	App  string            `json:"app,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Mode string            `json:"mode,omitempty"`
}

func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, request{Cmd: cmdPing}, nil)
}

// Start runs the app in place of every other running app, installing its dependencies first when they changed.
func (c *Client) Start(ctx context.Context, app string, env map[string]string) (StartResponse, error) {
	var resp StartResponse
	err := c.do(ctx, request{Cmd: cmdStart, App: app, Env: env, Mode: "replace"}, &resp)
	return resp, err
}

// Stop stops one app, or every app when app is empty. A non-running app is not an error.
func (c *Client) Stop(ctx context.Context, app string) (StopResponse, error) {
	var resp StopResponse
	err := c.do(ctx, request{Cmd: cmdStop, App: app}, &resp)
	return resp, err
}

func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var resp StatusResponse
	err := c.do(ctx, request{Cmd: cmdStatus}, &resp)
	return resp, err
}

func (c *Client) do(ctx context.Context, req request, resp any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return fmt.Errorf("cannot reach the app launcher: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}

	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("cannot send %s to the app launcher: %w", req.Cmd, err)
	}

	reply, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("no answer from the app launcher to %s: %w", req.Cmd, err)
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil {
		return fmt.Errorf("invalid answer from the app launcher: %w", err)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return &Error{Code: CodeInternal, Message: "request refused without a reason"}
		}
		return envelope.Error
	}
	if resp == nil {
		return nil
	}
	if err := json.Unmarshal(reply, resp); err != nil {
		return fmt.Errorf("invalid answer from the app launcher: %w", err)
	}
	return nil
}

func readLine(conn net.Conn) ([]byte, error) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), maxResponseBytes)
	if scanner.Scan() {
		return scanner.Bytes(), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("connection closed")
}

// IsCode tells whether err is a launcher error with that code.
func IsCode(err error, code string) bool {
	var launcherErr *Error
	return errors.As(err, &launcherErr) && launcherErr.Code == code
}
