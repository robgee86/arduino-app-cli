// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	dockerClient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

func TestGenerateIngressConf(t *testing.T) {
	t.Run("one server per port, to the slot by name", func(t *testing.T) {
		assert.Equal(t, `worker_processes 1;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events {}
stream {
    resolver 127.0.0.11 valid=5s ipv6=off;
    server {
        listen 7000;
        set $launcher slot3;
        proxy_pass $launcher:7000;
    }
    server {
        listen 8080;
        set $launcher slot3;
        proxy_pass $launcher:8080;
    }
}
`, string(generateIngressConf([]int{7000, 8080}, "slot3")))
	})

	t.Run("no ports, an empty stream", func(t *testing.T) {
		assert.Equal(t, "worker_processes 1;\nerror_log /dev/stderr warn;\npid /tmp/nginx.pid;\nevents {}\nstream {}\n", string(generateIngressConf(nil, "slot3")))
	})
}

func TestWriteIngressFiles(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)

	changed, err := writeIngressFiles(cfg, []int{8080, 7000, 8080}, "slot1")
	require.NoError(t, err)
	assert.Equal(t, ingressFiles{Conf: true, Compose: true}, changed)

	project := loadCompose(t, ingressComposeFile(cfg))
	assert.Equal(t, ingressProject, project.Name)
	assert.Equal(t, []string{ingressServiceName}, project.ServiceNames())
	svc := project.Services[ingressServiceName]

	assert.Equal(t, IngressImage, svc.Image)
	assert.True(t, *svc.Init)
	assert.Equal(t, "unless-stopped", svc.Restart)
	assert.Equal(t, "SIGTERM", svc.StopSignal)
	assert.Nil(t, svc.Entrypoint, "the image runs nginx")
	assert.Empty(t, svc.NetworkMode)
	assert.Equal(t, map[string]string{DockerIngressLabel: "true"}, map[string]string(svc.Labels), "no app label: the app cleanups leave it")

	t.Run("hardened", func(t *testing.T) {
		assert.Equal(t, "nginx", svc.User)
		assert.True(t, svc.ReadOnly)
		assert.Equal(t, []string{"/tmp"}, []string(svc.Tmpfs))
		assert.Equal(t, []string{"ALL"}, svc.CapDrop)
		assert.Empty(t, svc.CapAdd)
		assert.Equal(t, []string{"no-new-privileges:true"}, svc.SecurityOpt)
	})

	t.Run("publishes the ports, sorted once each", func(t *testing.T) {
		published := make([]string, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			published = append(published, fmt.Sprintf("%s:%d", p.Published, p.Target))
		}
		assert.Equal(t, []string{"7000:7000", "8080:8080"}, published)
		assert.Empty(t, svc.Ports[0].HostIP, "on every address, IPv6 included")
	})

	t.Run("on the launcher network, the conf mounted read-only", func(t *testing.T) {
		network := project.Networks["default"]
		assert.Equal(t, launcherNetwork, network.Name)
		assert.True(t, bool(network.External))

		require.Len(t, svc.Volumes, 1)
		assert.Equal(t, ingressConfFile(cfg).Parent().String(), svc.Volumes[0].Source)
		assert.Equal(t, "/etc/nginx", svc.Volumes[0].Target)
		assert.True(t, svc.Volumes[0].ReadOnly)

		conf, err := ingressConfFile(cfg).ReadFile()
		require.NoError(t, err)
		assert.Equal(t, generateIngressConf([]int{7000, 8080}, "slot1"), conf)
	})

	t.Run("the same ports change nothing", func(t *testing.T) {
		changed, err := writeIngressFiles(cfg, []int{7000, 8080}, "slot1")
		require.NoError(t, err)
		assert.Equal(t, ingressFiles{}, changed)
	})

	t.Run("a conf written by hand is noticed on its own", func(t *testing.T) {
		require.NoError(t, ingressConfFile(cfg).WriteFile([]byte("events {}\n")))
		changed, err := writeIngressFiles(cfg, []int{7000, 8080}, "slot1")
		require.NoError(t, err)
		assert.Equal(t, ingressFiles{Conf: true}, changed)
	})

	t.Run("no ports, nothing published", func(t *testing.T) {
		changed, err := writeIngressFiles(cfg, nil, "slot1")
		require.NoError(t, err)
		assert.Equal(t, ingressFiles{Conf: true, Compose: true}, changed)
		data, err := ingressComposeFile(cfg).ReadFile()
		require.NoError(t, err)
		assert.NotContains(t, string(data), "ports:")
		assert.Empty(t, loadCompose(t, ingressComposeFile(cfg)).Services[ingressServiceName].Ports)
		assertIngressConf(t, cfg, nil)
	})
}

const testIngressID = "ingress-id"

// fakeDocker answers the container lists with the containers of each label, and records the calls
// on the ingress. A started container runs, the images are present. Any other call goes to an
// engine that does not exist, so that a compose up fails.
type fakeDocker struct {
	dockerClient.APIClient
	byLabel map[string]container.Summary
	calls   []string
}

func newFakeDocker(t *testing.T, ingressState container.ContainerState) *fakeDocker {
	fake := newEmptyFakeDocker(t)
	fake.byLabel = map[string]container.Summary{DockerIngressLabel: {ID: testIngressID, State: ingressState}}
	return fake
}

func newEmptyFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	unreachable, err := dockerClient.New(dockerClient.WithHost("unix:///nonexistent/docker.sock"))
	require.NoError(t, err)
	return &fakeDocker{APIClient: unreachable}
}

func (f *fakeDocker) ContainerList(_ context.Context, options dockerClient.ContainerListOptions) (dockerClient.ContainerListResult, error) {
	for label := range options.Filters["label"] {
		if c, ok := f.byLabel[strings.TrimSuffix(label, "=true")]; ok {
			return dockerClient.ContainerListResult{Items: []container.Summary{c}}, nil
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

func (f *fakeDocker) ContainerInspect(context.Context, string, dockerClient.ContainerInspectOptions) (dockerClient.ContainerInspectResult, error) {
	return dockerClient.ContainerInspectResult{Container: container.InspectResponse{State: &container.State{Running: true}}}, nil
}

func (f *fakeDocker) ImageList(context.Context, dockerClient.ImageListOptions) (dockerClient.ImageListResult, error) {
	return dockerClient.ImageListResult{Items: []image.Summary{{RepoTags: []string{IngressImage}}}}, nil
}

// cli is a docker CLI on the fake.
func (f *fakeDocker) cli(t *testing.T) command.Cli {
	t.Helper()
	cli, err := command.NewDockerCli(command.WithAPIClient(f), command.WithBaseContext(t.Context()))
	require.NoError(t, err)
	require.NoError(t, cli.Initialize(&flags.ClientOptions{}))
	return cli
}

func TestOpenAppPorts(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	noop := func(StreamMessage) {}
	_, err := writeIngressFiles(cfg, []int{7000}, "slot1")
	require.NoError(t, err)

	t.Run("the same ports start the stopped ingress", func(t *testing.T) {
		docker := newFakeDocker(t, container.StateExited)
		require.NoError(t, openAppPorts(t.Context(), docker.cli(t), cfg, []int{7000}, "slot1", noop))
		assert.Equal(t, []string{"start " + testIngressID}, docker.calls)
	})

	t.Run("the same ports leave the running ingress as is", func(t *testing.T) {
		docker := newFakeDocker(t, container.StateRunning)
		require.NoError(t, openAppPorts(t.Context(), docker.cli(t), cfg, []int{7000}, "slot1", noop))
		assert.Empty(t, docker.calls)
	})

	t.Run("a conf that changed under the running ingress reloads it", func(t *testing.T) {
		// What an upgrade leaves behind: the same ports, a conf written by an older daemon
		require.NoError(t, ingressConfFile(cfg).WriteFile([]byte("events {}\n")))
		docker := newFakeDocker(t, container.StateRunning)
		require.NoError(t, openAppPorts(t.Context(), docker.cli(t), cfg, []int{7000}, "slot1", noop))
		assert.Equal(t, []string{"kill HUP " + testIngressID}, docker.calls)
		assertIngressConf(t, cfg, []int{7000})
	})

	t.Run("other ports recreate it with compose", func(t *testing.T) {
		docker := newFakeDocker(t, container.StateExited)
		require.ErrorContains(t, openAppPorts(t.Context(), docker.cli(t), cfg, []int{7000, 8080}, "slot1", noop), "failed to start the ingress")
		assert.Empty(t, docker.calls)
		assertIngressConf(t, cfg, []int{7000, 8080})
	})

	t.Run("no ingress yet creates it with compose", func(t *testing.T) {
		docker := newEmptyFakeDocker(t)
		require.ErrorContains(t, openAppPorts(t.Context(), docker.cli(t), cfg, []int{7000, 8080}, "slot1", noop), "failed to start the ingress")
		assert.Empty(t, docker.calls)
	})

	t.Run("no ports stop it, its file keeps the last ports", func(t *testing.T) {
		docker := newFakeDocker(t, container.StateRunning)
		require.NoError(t, openAppPorts(t.Context(), docker.cli(t), cfg, nil, "slot1", noop))
		assert.Equal(t, []string{"stop " + testIngressID}, docker.calls)
		assertIngressConf(t, cfg, []int{7000, 8080})
	})
}

func TestCloseAppPorts(t *testing.T) {
	for _, tc := range []struct {
		state     container.ContainerState
		wantCalls []string
	}{
		{state: container.StateRunning, wantCalls: []string{"stop " + testIngressID}},
		// A restart loop binds the ports at each attempt
		{state: container.StateRestarting, wantCalls: []string{"stop " + testIngressID}},
		{state: container.StateExited},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			docker := newFakeDocker(t, tc.state)
			require.NoError(t, closeAppPorts(t.Context(), docker))
			assert.Equal(t, tc.wantCalls, docker.calls)
		})
	}

	t.Run("no ingress", func(t *testing.T) {
		docker := newEmptyFakeDocker(t)
		require.NoError(t, closeAppPorts(t.Context(), docker))
		assert.Empty(t, docker.calls)
	})
}

func TestPullIngressImage(t *testing.T) {
	present := newEmptyFakeDocker(t)
	require.NoError(t, pullIngressImage(t.Context(), present, func(StreamMessage) {}))
	assert.Empty(t, present.calls, "an image the board has is not pulled")
}

func TestStopAppProcessesClosesThePortsOfItsApp(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	require.NoError(t, cfg.RuntimeDir().MkdirAll())
	_, err := writeIngressFiles(cfg, []int{7000}, "slot1")
	require.NoError(t, err)
	// The slot is unreachable: it runs nothing
	stopped := newTestApp(t, cfg, "stopped")
	stopped.MainPythonFile = stopped.FullPath.Join("python", "main.py")

	t.Run("another app keeps its ports", func(t *testing.T) {
		docker := newFakeDocker(t, container.StateRunning)
		writeTestSlot(t, cfg, slot{ID: 1, App: cfg.AppsDir().Join("other").String()})
		require.NoError(t, stopAppProcesses(t.Context(), docker, stopped, cfg, nil))
		assert.Empty(t, docker.calls)
	})

	t.Run("its own ports close", func(t *testing.T) {
		docker := newFakeDocker(t, container.StateRunning)
		writeTestSlot(t, cfg, slot{ID: 1, App: stopped.FullPath.String()})
		require.NoError(t, stopAppProcesses(t.Context(), docker, stopped, cfg, nil))
		assert.Equal(t, []string{"stop " + testIngressID}, docker.calls)
		_, running := activeSlot(cfg, nil)
		assert.False(t, running, "its slot is retired")
		assertIngressConf(t, cfg, []int{7000})
	})
}

// assertIngressConf checks that the ingress conf forwards the ports.
func assertIngressConf(t *testing.T, cfg config.Configuration, ports []int) {
	t.Helper()
	conf, err := ingressConfFile(cfg).ReadFile()
	require.NoError(t, err)
	assert.Equal(t, string(generateIngressConf(ports, "slot1")), string(conf))
}
