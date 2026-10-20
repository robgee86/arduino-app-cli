// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// fakeDocker answers the container calls of the orchestrator from a list of containers by label and
// records what it is asked to do. Anything else reaches an unreachable daemon.
type fakeDocker struct {
	dockerClient.APIClient
	byLabel map[string][]container.Summary
	calls   []string
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	unreachable, err := dockerClient.New(dockerClient.WithHost("unix:///nonexistent/docker.sock"))
	require.NoError(t, err)
	return &fakeDocker{APIClient: unreachable}
}

func (f *fakeDocker) ContainerList(_ context.Context, options dockerClient.ContainerListOptions) (dockerClient.ContainerListResult, error) {
	for label := range options.Filters["label"] {
		if containers, ok := f.byLabel[strings.TrimSuffix(label, "=true")]; ok {
			return dockerClient.ContainerListResult{Items: containers}, nil
		}
	}
	return dockerClient.ContainerListResult{}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ dockerClient.ContainerStartOptions) (dockerClient.ContainerStartResult, error) {
	f.calls = append(f.calls, "start "+id)
	return dockerClient.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ dockerClient.ContainerStopOptions) (dockerClient.ContainerStopResult, error) {
	f.calls = append(f.calls, "stop "+id)
	return dockerClient.ContainerStopResult{}, nil
}

func (f *fakeDocker) ContainerKill(_ context.Context, id string, options dockerClient.ContainerKillOptions) (dockerClient.ContainerKillResult, error) {
	f.calls = append(f.calls, "kill "+options.Signal+" "+id)
	return dockerClient.ContainerKillResult{}, nil
}

// ContainerRestart finds no app container: the tests run none.
func (f *fakeDocker) ContainerRestart(_ context.Context, id string, _ dockerClient.ContainerRestartOptions) (dockerClient.ContainerRestartResult, error) {
	f.calls = append(f.calls, "restart "+id)
	return dockerClient.ContainerRestartResult{}, cerrdefs.ErrNotFound
}

func (f *fakeDocker) ContainerInspect(context.Context, string, dockerClient.ContainerInspectOptions) (dockerClient.ContainerInspectResult, error) {
	return dockerClient.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: true}}}, nil
}
