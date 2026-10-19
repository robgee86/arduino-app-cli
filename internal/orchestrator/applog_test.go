// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func collectAppLog(ctx context.Context, appDir *paths.Path, tail *uint64, follow bool, limit int) []string {
	var lines []string
	readAppLogFile(ctx, appDir, tail, follow, func(msg LogMessage) bool {
		lines = append(lines, msg.Content)
		return len(lines) < limit
	})
	return lines
}

func appendTo(t *testing.T, file *paths.Path, data string) {
	t.Helper()
	require.NoError(t, file.Parent().MkdirAll())
	f, err := os.OpenFile(file.String(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestReadAppLogFile(t *testing.T) {
	newApp := func(t *testing.T) (appDir, logFile *paths.Path) {
		appDir = paths.New(t.TempDir())
		return appDir, appDir.Join(appLogFile)
	}
	tail := func(n uint64) *uint64 { return &n }

	t.Run("no log yet", func(t *testing.T) {
		appDir, _ := newApp(t)
		assert.Empty(t, collectAppLog(t.Context(), appDir, nil, false, 100))
	})

	t.Run("the rotated log comes first", func(t *testing.T) {
		appDir, logFile := newApp(t)
		appendTo(t, paths.New(logFile.String()+".1"), "one\ntwo\n")
		appendTo(t, logFile, "three\n\nfive")
		assert.Equal(t, []string{"one", "two", "three", "", "five"}, collectAppLog(t.Context(), appDir, nil, false, 100))
		assert.Equal(t, []string{"", "five"}, collectAppLog(t.Context(), appDir, tail(2), false, 100))
	})

	t.Run("a link leading out of the app folder is not followed", func(t *testing.T) {
		appDir, logFile := newApp(t)
		outside := paths.New(t.TempDir()).Join("secret")
		require.NoError(t, outside.WriteFile([]byte("secret\n")))
		require.NoError(t, logFile.Parent().MkdirAll())
		require.NoError(t, os.Symlink(outside.String(), logFile.String()))
		assert.Empty(t, collectAppLog(t.Context(), appDir, nil, false, 100))
	})

	t.Run("follow across a rotation", func(t *testing.T) {
		appDir, logFile := newApp(t)
		appendTo(t, logFile, "old\nstart of ")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan []string)
		go func() { done <- collectAppLog(ctx, appDir, tail(1), true, 5) }()

		time.Sleep(2 * appLogPollInterval)
		appendTo(t, logFile, "a line\nbefore rotation\n")
		time.Sleep(2 * appLogPollInterval)
		appendTo(t, logFile, "last of old file\n")
		require.NoError(t, logFile.Rename(paths.New(logFile.String()+".1")))
		appendTo(t, logFile, "new file\n")

		assert.Equal(t, []string{"old", "start of a line", "before rotation", "last of old file", "new file"}, <-done)
	})

	t.Run("follow stops with the context", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*appLogPollInterval)
		defer cancel()
		appDir, _ := newApp(t)
		assert.Empty(t, collectAppLog(ctx, appDir, nil, true, 100))
	})
}
