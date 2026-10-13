// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testApp = "/apps/a"

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
			return `{"ok":true,"pid":42,"started_at":"2026-10-13T10:00:00.5+00:00","warm":true,"runner":"python","timings":{"stop_ms":12.5,"fork_ms":3}}`
		case cmdStop:
			return `{"ok":true,"stopped":[{"app":"/apps/a","pid":42,"exit_code":null,"signal":15}]}`
		case cmdStatus:
			return `{"ok":true,"children":[{"app":"/apps/a","pid":42,"state":"running","started_at":"2026-10-13T10:00:00+00:00","http_port":17000,"runner":"python","warm":true}],"preload":{"modules":["numpy"]},"version":"1.0","uptime_s":3.5}`
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

	t.Run("start replaces with the given env", func(t *testing.T) {
		resp, err := client.Start(ctx, testApp, map[string]string{"FOO": "bar"})
		require.NoError(t, err)
		assert.Equal(t, request{Cmd: cmdStart, App: testApp, Env: map[string]string{"FOO": "bar"}, Mode: "replace"}, <-fake.requests)
		assert.Equal(t, 42, resp.PID)
		assert.True(t, resp.Warm)
		assert.Equal(t, "python", resp.Runner)
		assert.Equal(t, Timings{StopMs: 12.5, ForkMs: 3}, resp.Timings)
		assert.Equal(t, time.Date(2026, 10, 13, 10, 0, 0, 5e8, time.UTC), resp.StartedAt.UTC())
	})

	t.Run("stop one app", func(t *testing.T) {
		resp, err := client.Stop(ctx, testApp)
		require.NoError(t, err)
		assert.Equal(t, request{Cmd: cmdStop, App: testApp}, <-fake.requests)
		require.Len(t, resp.Stopped, 1)
		assert.Nil(t, resp.Stopped[0].ExitCode)
		assert.Equal(t, 15, *resp.Stopped[0].Signal)
	})

	t.Run("stop every app", func(t *testing.T) {
		_, err := client.Stop(ctx, "")
		require.NoError(t, err)
		assert.Equal(t, request{Cmd: cmdStop}, <-fake.requests)
	})

	t.Run("status", func(t *testing.T) {
		resp, err := client.Status(ctx)
		require.NoError(t, err)
		<-fake.requests
		assert.Equal(t, []Child{{
			App: testApp, PID: 42, State: StateRunning, StartedAt: resp.Children[0].StartedAt,
			HTTPPort: 17000, Runner: "python", Warm: true,
		}}, resp.Children)
		assert.Equal(t, "1.0", resp.Version)
		assert.InDelta(t, 3.5, resp.UptimeS, 0)
	})
}

func TestClientErrors(t *testing.T) {
	t.Run("a refusal carries its code", func(t *testing.T) {
		fake := newFakeLauncher(t, func(string) string {
			return `{"ok":false,"error":{"code":"unsupported","message":"use mode replace"}}`
		})
		_, err := New(fake.socket, time.Second).Start(context.Background(), testApp, nil)
		var launcherErr *Error
		require.ErrorAs(t, err, &launcherErr)
		assert.Equal(t, CodeUnsupported, launcherErr.Code)
		assert.Equal(t, "use mode replace", launcherErr.Message)
		assert.True(t, IsCode(err, CodeUnsupported))
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
