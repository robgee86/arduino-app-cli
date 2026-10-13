// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"testing"

	"github.com/arduino/go-paths-helper"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

const (
	appA = "/apps/a"
	appB = "/apps/b"
)

func TestMergeAppStatus(t *testing.T) {
	sidecar := func(appPath string, state container.ContainerState, status string) container.Summary {
		return container.Summary{State: state, Status: status, Labels: map[string]string{DockerAppLabel: "true", DockerAppPathLabel: appPath}}
	}
	main := func(appPath string, state container.ContainerState, status string) container.Summary {
		c := sidecar(appPath, state, status)
		c.Labels[DockerAppMainLabel] = "true"
		return c
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
			containers: []container.Summary{sidecar(appA, container.StateRunning, "Up")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}},
			want:       map[string]Status{appA: StatusRunning},
		},
		{
			name:       "a child with restarting sidecars starts",
			containers: []container.Summary{sidecar(appA, container.StateRunning, "Up"), sidecar(appA, container.StateRestarting, "Restarting")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}},
			want:       map[string]Status{appA: StatusStarting},
		},
		{
			name:       "a failed sidecar fails the app",
			containers: []container.Summary{sidecar(appA, container.StateExited, "Exited (1) 3 seconds ago")},
			view:       launcherView{children: []launcher.Child{child(appA, launcher.StateRunning)}},
			want:       map[string]Status{appA: StatusFailed},
		},
		{
			name:       "a failed sidecar fails the wanted app whose child is gone",
			containers: []container.Summary{sidecar(appA, container.StateExited, "Exited (1) 3 seconds ago")},
			view:       launcherView{wanted: paths.New(appA)},
			want:       map[string]Status{appA: StatusFailed},
		},
		{
			name:       "a stopped app whose runner exited 0 on SIGTERM is stopped",
			containers: []container.Summary{sidecar(appA, container.StateExited, "Exited (0) 5 seconds ago")},
			want:       map[string]Status{appA: StatusStopped},
		},
		{
			name: "a child being stopped stops",
			view: launcherView{children: []launcher.Child{child(appA, launcher.StateStopping)}},
			want: map[string]Status{appA: StatusStopping},
		},
		{
			name:       "running sidecars without child are stopped",
			containers: []container.Summary{sidecar(appA, container.StateRunning, "Up")},
			want:       map[string]Status{appA: StatusStopped},
		},
		{
			name: "a wanted app without child failed",
			view: launcherView{wanted: paths.New(appA)},
			want: map[string]Status{appA: StatusFailed},
		},
		{
			name:       "an app with a main container keeps the container status",
			containers: []container.Summary{main(appA, container.StateRunning, "Up"), sidecar(appA, container.StateRunning, "Up")},
			want:       map[string]Status{appA: StatusRunning},
		},
		{
			name:       "a replaced app stops while the new one runs",
			containers: []container.Summary{sidecar(appA, container.StateExited, "Exited (143) 1 second ago"), sidecar(appB, container.StateRunning, "Up")},
			view:       launcherView{children: []launcher.Child{child(appB, launcher.StateRunning)}, wanted: paths.New(appB)},
			want:       map[string]Status{appA: StatusStopped, appB: StatusRunning},
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
	last := map[string]Status{appA: StatusRunning, appB: StatusStopped, "/apps/c": StatusRunning}
	current := map[string]Status{appA: StatusStopped, appB: StatusStopped, "/apps/d": StatusRunning}

	changes := statusChanges(last, current)
	got := make([]string, 0, len(changes))
	for _, c := range changes {
		got = append(got, c.AppPath.String()+"="+string(c.Status))
	}
	assert.Equal(t, []string{"/apps/a=stopped", "/apps/c=uninitialized", "/apps/d=running"}, got)
	assert.Empty(t, statusChanges(current, current))
}
