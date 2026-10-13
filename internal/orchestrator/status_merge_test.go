// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/arduino/go-paths-helper"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

func TestMergeAppStatus(t *testing.T) {
	root := t.TempDir()
	// a and b were provisioned, fresh was not
	appA, appB, fresh := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "fresh")
	require.NoError(t, paths.New(appA, ".cache").MkdirAll())
	require.NoError(t, paths.New(appB, ".cache").MkdirAll())

	services := 0
	named := func(project, service string, state container.ContainerState, status string) container.Summary {
		return container.Summary{State: state, Status: status, Labels: map[string]string{
			DockerAppLabel: "true", composeProjectLabel: project, composeServiceLabel: service,
		}}
	}
	sidecar := func(project string, state container.ContainerState, status string) container.Summary {
		services++
		return named(project, fmt.Sprintf("runner-%d", services), state, status)
	}
	zone := func(state container.ContainerState, status string) container.Summary {
		return sidecar(sidecarsProject, state, status)
	}
	child := func(appPath, state string) launcher.Child {
		return launcher.Child{App: appPath, State: state}
	}

	tests := []struct {
		name       string
		containers []container.Summary
		view       launcherView
		want       map[string]Status
	}{
		{
			name: "a child without sidecars runs",
			view: launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}, wanted: paths.New(appA)},
			want: map[string]Status{appA: StatusRunning},
		},
		{
			name:       "a child with running sidecars runs",
			containers: []container.Summary{zone(container.StateRunning, "Up")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}, wanted: paths.New(appA)},
			want:       map[string]Status{appA: StatusRunning},
		},
		{
			name:       "a child with restarting sidecars starts",
			containers: []container.Summary{zone(container.StateRunning, "Up"), zone(container.StateRestarting, "Restarting")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}, wanted: paths.New(appA)},
			want:       map[string]Status{appA: StatusStarting},
		},
		{
			name:       "a failed sidecar fails the app",
			containers: []container.Summary{zone(container.StateExited, "Exited (1) 3 seconds ago")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}, wanted: paths.New(appA)},
			want:       map[string]Status{appA: StatusFailed},
		},
		{
			name:       "a failed sidecar fails the wanted app whose child is gone",
			containers: []container.Summary{zone(container.StateExited, "Exited (1) 3 seconds ago")},
			view:       launcherView{wanted: paths.New(appA)},
			want:       map[string]Status{appA: StatusFailed},
		},
		{
			name:       "the zone runs the sidecars of the wanted app only",
			containers: []container.Summary{zone(container.StateRunning, "Up"), zone(container.StateExited, "Exited (1) 3 seconds ago")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning), child(appB, launcher.StateRunning)}, wanted: paths.New(appB)},
			want:       map[string]Status{appA: StatusRunning, appB: StatusFailed},
		},
		{
			name:       "an orphan removed alongside the start is no sidecar of the app",
			containers: []container.Summary{named(sidecarsProject, "kept", container.StateRestarting, "Restarting"), named(sidecarsProject, "orphan", container.StateExited, "Exited (0) 1 second ago")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}, wanted: paths.New(appA), sidecars: []string{"kept"}},
			want:       map[string]Status{appA: StatusStarting},
		},
		{
			name:       "the containers of the per-app projects are not sidecars",
			containers: []container.Summary{sidecar("a", container.StateExited, "Exited (1) 3 seconds ago")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}, wanted: paths.New(appA)},
			want:       map[string]Status{appA: StatusRunning},
		},
		{
			name:       "a stopped zone whose runner exited 0 on SIGTERM tells nothing",
			containers: []container.Summary{zone(container.StateExited, "Exited (0) 5 seconds ago")},
			want:       map[string]Status{},
		},
		{
			name: "a child being stopped stops",
			view: launcherView{children: []launcher.Child{child(appA, launcher.StateStopping)}},
			want: map[string]Status{appA: StatusStopping},
		},
		{
			name:       "running sidecars without wanted app run no app",
			containers: []container.Summary{zone(container.StateRunning, "Up")},
			want:       map[string]Status{},
		},
		{
			name: "a wanted app without child failed",
			view: launcherView{wanted: paths.New(fresh)},
			want: map[string]Status{fresh: StatusFailed},
		},
		{
			name: "nothing runs",
			want: map[string]Status{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, statusByPath(mergeAppStatus(tc.containers, tc.view)))
		})
	}
}

func TestStatusChanges(t *testing.T) {
	provisioned := filepath.Join(t.TempDir(), "provisioned")
	require.NoError(t, paths.New(provisioned, ".cache").MkdirAll())
	const appA, appB, gone, added = "/apps/a", "/apps/b", "/apps/c", "/apps/d"

	last := map[string]Status{appA: StatusRunning, appB: StatusStopped, gone: StatusRunning, provisioned: StatusRunning}
	current := map[string]Status{appA: StatusStopped, appB: StatusStopped, added: StatusRunning}

	changes := statusChanges(last, current)
	got := make([]string, 0, len(changes))
	for _, c := range changes {
		got = append(got, c.AppPath.String()+"="+string(c.Status))
	}
	// A gone app is stopped when it has a cache, uninitialized otherwise
	assert.ElementsMatch(t, []string{"/apps/a=stopped", "/apps/c=uninitialized", "/apps/d=running", provisioned + "=stopped"}, got)
	assert.Empty(t, statusChanges(current, current))
	assert.Empty(t, statusChanges(map[string]Status{provisioned: StatusStopped}, map[string]Status{}), "a stopped app that is gone stays stopped")
}
