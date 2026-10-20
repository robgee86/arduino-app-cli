// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
)

func TestSidecarsStatus(t *testing.T) {
	tests := []struct {
		name           string
		containerState []container.ContainerState
		statusMessage  []string
		want           Status
	}{
		{
			name:           "everything running",
			containerState: []container.ContainerState{container.StateRunning, container.StateRunning},
			statusMessage:  []string{"Up 5 minutes", "Up 10 minutes"},
			want:           StatusRunning,
		},
		{
			name:           "everything stopped",
			containerState: []container.ContainerState{container.StateCreated, container.StatePaused, container.StateExited},
			statusMessage:  []string{"Created", "Paused", "Exited (137)"},
			want:           StatusStopped,
		},
		{
			name:           "failed container",
			containerState: []container.ContainerState{container.StateRunning, container.StateDead},
			statusMessage:  []string{"Up 5 minutes", "Dead"},
			want:           StatusFailed,
		},
		{
			name:           "failed container takes precedence over stopping and starting",
			containerState: []container.ContainerState{container.StateRunning, container.StateDead, container.StateRemoving, container.StateRestarting},
			statusMessage:  []string{"Up 5 minutes", "Dead", "Removing", "Restarting"},
			want:           StatusFailed,
		},
		{
			name:           "stopping",
			containerState: []container.ContainerState{container.StateRunning, container.StateRemoving},
			statusMessage:  []string{"Up 5 minutes", "Removing"},
			want:           StatusStopping,
		},
		{
			name:           "stopping takes precedence over starting",
			containerState: []container.ContainerState{container.StateRunning, container.StateRestarting, container.StateRemoving},
			statusMessage:  []string{"Up 5 minutes", "Restarting", "Removing"},
			want:           StatusStopping,
		},
		{
			name:           "starting",
			containerState: []container.ContainerState{container.StateRestarting, container.StateExited},
			statusMessage:  []string{"Restarting", "Exited (129)"},
			want:           StatusStarting,
		},
		{
			name:           "failed",
			containerState: []container.ContainerState{container.StateRestarting, container.StateExited},
			statusMessage:  []string{"Restarting", "Exited (1)"},
			want:           StatusFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The main container of the app and the sidecars of another app do not count
			input := []container.Summary{
				{Labels: map[string]string{composeProjectLabel: "my-app", composeServiceLabel: mainServiceName, DockerAppMainLabel: "true"}, State: container.StateDead},
				{Labels: map[string]string{composeProjectLabel: "other", composeServiceLabel: "svc"}, State: container.StateDead},
			}
			for i, c := range tc.containerState {
				input = append(input, container.Summary{
					Labels: map[string]string{composeProjectLabel: "my-app", composeServiceLabel: fmt.Sprintf("svc%d", i)},
					State:  c,
					Status: tc.statusMessage[i],
				})
			}
			require.Equal(t, tc.want, sidecarsStatus(input, "my-app"))
		})
	}
}
