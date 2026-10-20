// SPDX-FileCopyrightText: Copyright (C) Arduino s.r.l. and/or its affiliated companies
//
// SPDX-License-Identifier: MPL-2.0

package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProc writes a process table: pid -> parent.
func fakeProc(t *testing.T, tree map[int]int) {
	t.Helper()
	root := t.TempDir()
	for pid, parent := range tree {
		dir := filepath.Join(root, strconv.Itoa(pid))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), fmt.Appendf(nil, "Name:\tx\nPid:\t%d\nPPid:\t%d\n", pid, parent), 0o600))
	}
	previous := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = previous })
}

func TestProcessTree(t *testing.T) {
	// init 100 of a container: supervisor 101, its app 102 with a helper 103, an orphan 104 reparented to init
	fakeProc(t, map[int]int{1: 0, 100: 1, 101: 100, 102: 101, 103: 102, 104: 100, 200: 1})

	parent, err := parentPID(101)
	require.NoError(t, err)
	assert.Equal(t, 100, parent)
	_, err = parentPID(999)
	assert.Error(t, err, "a gone process has no parent")

	tree, err := processTree()
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{101, 102, 103, 104}, descendants(tree, 100))
	assert.ElementsMatch(t, []int{101}, descendants(map[int]int{100: 1, 101: 100}, 100), "a quiet container holds the supervisor only")
	assert.Empty(t, descendants(tree, 200))
}

func TestReservedVariablesCoverInstallerAndProxies(t *testing.T) {
	for _, name := range []string{"UV_INDEX_URL", "PIP_INDEX_URL", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "LD_PRELOAD"} {
		assert.True(t, reservedVariable(name), name)
	}
	for _, name := range []string{"TOKEN", "MODEL", "UVICORN_PORT"} {
		assert.False(t, reservedVariable(name), name)
	}
}
