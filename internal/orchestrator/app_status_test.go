// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

func TestFollowLauncherTellsItsChildrenAndWhenItGivesNoEvents(t *testing.T) {
	const running = `{"ok":true,"children":[{"app":"/apps/a","pid":42,"state":"running"}]}`
	const none = `{"ok":true,"children":[]}`
	// What the launcher answers to each connection in turn: the last one is kept open
	answers := []string{
		running + "\n" + none + "\n",
		`{"ok":false,"error":{"code":"bad_request","message":"unknown cmd: 'events'"}}` + "\n",
		running + "\n",
	}

	// macOS limits socket paths to 104 bytes: t.TempDir() is often longer
	dir, err := os.MkdirTemp("", "l")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, launcher.SocketName)
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for i, answer := range answers {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if _, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil {
				_, _ = conn.Write([]byte(answer))
			}
			if i == len(answers)-1 {
				<-done
			}
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates := followLauncher(ctx, launcher.New(socket, time.Second), 10*time.Millisecond)
	next := func() launcherChildren {
		select {
		case update := <-updates:
			return update
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the launcher told nothing")
			return launcherChildren{}
		}
	}
	apps := func(update launcherChildren) []string {
		result := make([]string, 0, len(update.children))
		for _, child := range update.children {
			result = append(result, child.App+" "+child.State)
		}
		return result
	}

	first := next()
	assert.True(t, first.followed)
	assert.Equal(t, []string{"/apps/a running"}, apps(first), "the apps that run now come first")
	second := next()
	assert.True(t, second.followed)
	assert.Empty(t, apps(second), "then each change")

	assert.Equal(t, launcherChildren{}, next(), "a launcher that went is asked instead")
	assert.Equal(t, launcherChildren{}, next(), "as one that has no events")

	back := next()
	assert.True(t, back.followed, "it is followed again once it gives its events")
	assert.Equal(t, []string{"/apps/a running"}, apps(back))
}
