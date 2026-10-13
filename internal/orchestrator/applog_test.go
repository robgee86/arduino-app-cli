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

func collectAppLog(ctx context.Context, logFile *paths.Path, tail *uint64, follow bool, limit int) []string {
	var lines []string
	readAppLogFile(ctx, logFile, tail, follow, func(msg LogMessage) bool {
		lines = append(lines, msg.Content)
		return len(lines) < limit
	})
	return lines
}

func appendTo(t *testing.T, file *paths.Path, data string) {
	t.Helper()
	f, err := os.OpenFile(file.String(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestReadAppLogFile(t *testing.T) {
	newLog := func(t *testing.T) *paths.Path {
		dir := paths.New(t.TempDir())
		return dir.Join("app.log")
	}
	tail := func(n uint64) *uint64 { return &n }

	t.Run("no log yet", func(t *testing.T) {
		assert.Empty(t, collectAppLog(t.Context(), newLog(t), nil, false, 100))
	})

	t.Run("the rotated log comes first", func(t *testing.T) {
		logFile := newLog(t)
		appendTo(t, paths.New(logFile.String()+".1"), "one\ntwo\n")
		appendTo(t, logFile, "three\n\nfive")
		assert.Equal(t, []string{"one", "two", "three", "", "five"}, collectAppLog(t.Context(), logFile, nil, false, 100))
		assert.Equal(t, []string{"", "five"}, collectAppLog(t.Context(), logFile, tail(2), false, 100))
	})

	t.Run("follow across a rotation", func(t *testing.T) {
		logFile := newLog(t)
		appendTo(t, logFile, "old\nstart of ")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan []string)
		go func() { done <- collectAppLog(ctx, logFile, tail(1), true, 5) }()

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
		assert.Empty(t, collectAppLog(ctx, newLog(t), nil, true, 100))
	})
}
