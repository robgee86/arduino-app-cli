// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testApp = "/app"

// fakeLauncher answers every connection with the reply of handle, recording the requests.
type fakeLauncher struct {
	socket   string
	requests chan request
}

func newFakeLauncher(t *testing.T, handle func(cmd string) string) *fakeLauncher {
	t.Helper()
	// macOS limits socket paths to 104 bytes: t.TempDir() is often longer
	dir, err := os.MkdirTemp("", "l")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, SocketName)

	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	fake := &fakeLauncher{socket: socket, requests: make(chan request, 16)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req request
				if json.Unmarshal(line, &req) != nil {
					return
				}
				fake.requests <- req
				if reply := handle(req.Cmd); reply != "" {
					_, _ = conn.Write([]byte(reply + "\n"))
				}
			}()
		}
	}()
	return fake
}

func TestClientRequests(t *testing.T) {
	fake := newFakeLauncher(t, func(cmd string) string {
		switch cmd {
		case cmdStart:
			return `{"ok":true,"pid":42,"started_at":"2026-10-13T10:00:00.5+00:00","warm":true,"runner":"python","prepared":true,"timings":{"stop_ms":12.5,"prepare_ms":2500,"fork_ms":3}}`
		case cmdStop:
			return `{"ok":true,"stopped":[{"app":"/app","pid":42,"exit_code":null,"signal":15}]}`
		case cmdStatus:
			return `{"ok":true,"child":{"app":"/app","pid":42,"started_at":"2026-10-13T10:00:00+00:00","runner":"python","warm":true,"stopping":false},"preload":{"modules":["numpy"]},"version":"1.0","uptime_s":3.5}`
		default:
			return `{"ok":true}`
		}
	})
	client := New(fake.socket, time.Second)
	ctx := context.Background()

	t.Run("ping", func(t *testing.T) {
		require.NoError(t, client.Ping(ctx))
		assert.Equal(t, request{Cmd: cmdPing}, <-fake.requests)
	})

	t.Run("start with the app environment", func(t *testing.T) {
		resp, err := client.Start(ctx, testApp, map[string]string{"FOO": "bar"})
		require.NoError(t, err)
		assert.Equal(t, request{Cmd: cmdStart, App: testApp, Env: map[string]string{"FOO": "bar"}}, <-fake.requests)
		assert.Equal(t, 42, resp.PID)
		assert.True(t, resp.Warm)
		assert.Equal(t, "python", resp.Runner)
		assert.True(t, resp.Prepared)
		assert.Equal(t, Timings{StopMs: 12.5, PrepareMs: 2500, ForkMs: 3}, resp.Timings)
		assert.Equal(t, time.Date(2026, 10, 13, 10, 0, 0, 5e8, time.UTC), resp.StartedAt.UTC())
	})

	t.Run("start without environment sends none", func(t *testing.T) {
		_, err := client.Start(ctx, testApp, nil)
		require.NoError(t, err)
		assert.Equal(t, request{Cmd: cmdStart, App: testApp}, <-fake.requests)
	})

	t.Run("stop", func(t *testing.T) {
		resp, err := client.Stop(ctx)
		require.NoError(t, err)
		assert.Equal(t, request{Cmd: cmdStop}, <-fake.requests)
		require.Len(t, resp.Stopped, 1)
		assert.Nil(t, resp.Stopped[0].ExitCode)
		assert.Equal(t, 15, *resp.Stopped[0].Signal)
	})

	t.Run("status", func(t *testing.T) {
		resp, err := client.Status(ctx)
		require.NoError(t, err)
		<-fake.requests
		require.NotNil(t, resp.Child)
		assert.Equal(t, Child{App: testApp, PID: 42, StartedAt: resp.Child.StartedAt, Runner: "python", Warm: true}, *resp.Child)
		assert.Equal(t, "1.0", resp.Version)
		assert.InDelta(t, 3.5, resp.UptimeS, 0)
	})
}

func TestClientStatusWithoutChild(t *testing.T) {
	fake := newFakeLauncher(t, func(string) string { return `{"ok":true,"child":null,"version":"1.0"}` })
	resp, err := New(fake.socket, time.Second).Status(context.Background())
	require.NoError(t, err)
	assert.Nil(t, resp.Child)
}

func TestClientErrors(t *testing.T) {
	t.Run("a refusal carries its code", func(t *testing.T) {
		fake := newFakeLauncher(t, func(string) string {
			return `{"ok":false,"error":{"code":"busy","message":"a start is in progress"}}`
		})
		_, err := New(fake.socket, time.Second).Start(context.Background(), testApp, nil)
		var launcherErr *Error
		require.ErrorAs(t, err, &launcherErr)
		assert.Equal(t, CodeBusy, launcherErr.Code)
		assert.Equal(t, "a start is in progress", launcherErr.Message)
		assert.True(t, IsCode(err, CodeBusy))
	})

	t.Run("a failed dependency install", func(t *testing.T) {
		fake := newFakeLauncher(t, func(string) string {
			return `{"ok":false,"error":{"code":"prepare_failed","message":"pip failed\nERROR: no nump"}}`
		})
		_, err := New(fake.socket, time.Second).Start(context.Background(), testApp, nil)
		var launcherErr *Error
		require.ErrorAs(t, err, &launcherErr)
		assert.Equal(t, CodePrepareFailed, launcherErr.Code)
		assert.Equal(t, "pip failed\nERROR: no nump", launcherErr.Message)
	})

	t.Run("no socket", func(t *testing.T) {
		err := New(filepath.Join(t.TempDir(), "missing.sock"), time.Second).Ping(context.Background())
		require.ErrorContains(t, err, "cannot reach the app launcher")
		assert.False(t, IsCode(err, CodeInternal))
	})

	t.Run("no answer within the timeout", func(t *testing.T) {
		fake := newFakeLauncher(t, func(string) string {
			time.Sleep(time.Second)
			return ""
		})
		started := time.Now()
		err := New(fake.socket, 100*time.Millisecond).Ping(context.Background())
		require.ErrorContains(t, err, "no answer from the app launcher")
		assert.Less(t, time.Since(started), 500*time.Millisecond)
	})

	t.Run("invalid answer", func(t *testing.T) {
		fake := newFakeLauncher(t, func(string) string { return "not json" })
		require.ErrorContains(t, New(fake.socket, time.Second).Ping(context.Background()), "invalid answer")
	})
}

func TestClientAcceptsOnlyTheApprovedPeer(t *testing.T) {
	fake := newFakeLauncher(t, func(string) string { return `{"ok":true}` })
	previous := peerPID
	peerPID = func(net.Conn) (int, error) { return 4242, nil }
	t.Cleanup(func() { peerPID = previous })

	var seen int
	approve := func(pid int) error { seen = pid; return nil }
	require.NoError(t, New(fake.socket, time.Second).WithPeer(approve).Ping(context.Background()))
	assert.Equal(t, 4242, seen, "the check sees the peer pid")
	<-fake.requests

	refuse := func(int) error { return errors.New("not the supervisor") }
	err := New(fake.socket, time.Second).WithPeer(refuse).WithTimeout(time.Second).Ping(context.Background())
	require.ErrorIs(t, err, ErrPeer)
	assert.Equal(t, 0, len(fake.requests), "a refused peer is sent nothing")
}
