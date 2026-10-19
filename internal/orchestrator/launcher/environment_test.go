// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package launcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteEnvironment(t *testing.T) {
	t.Run("readable by the app uid only", func(t *testing.T) {
		appDir := t.TempDir()
		require.NoError(t, WriteEnvironment(appDir, map[string]string{"AUTH_KEY": "s3cret", "PORT": "7000"}))
		file := filepath.Join(appDir, EnvironmentFile)
		info, err := os.Stat(file)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.JSONEq(t, `{"AUTH_KEY":"s3cret","PORT":"7000"}`, string(data))
	})

	t.Run("a link leading out of the app folder is not followed", func(t *testing.T) {
		appDir, outside := t.TempDir(), t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(appDir, ".cache")))
		require.Error(t, WriteEnvironment(appDir, map[string]string{"AUTH_KEY": "s3cret"}))
		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}
