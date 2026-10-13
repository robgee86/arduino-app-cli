// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

func TestGenerateIngressConf(t *testing.T) {
	t.Run("one server per port, to the launcher by name", func(t *testing.T) {
		assert.Equal(t, `worker_processes 1;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events {}
stream {
    resolver 127.0.0.11 valid=5s ipv6=off;
    server {
        listen 7000;
        set $launcher launcher;
        proxy_pass $launcher:17000;
    }
    server {
        listen 8080;
        set $launcher launcher;
        proxy_pass $launcher:18080;
    }
}
`, string(generateIngressConf([]int{7000, 8080})))
	})

	t.Run("no ports, an empty stream", func(t *testing.T) {
		assert.Equal(t, "worker_processes 1;\nerror_log /dev/stderr warn;\npid /tmp/nginx.pid;\nevents {}\nstream {}\n", string(generateIngressConf(nil)))
	})
}

func TestWriteIngressFiles(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)

	changed, err := writeIngressFiles(cfg, []int{8080, 7000, 8080})
	require.NoError(t, err)
	assert.True(t, changed)

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
		assert.Equal(t, generateIngressConf([]int{7000, 8080}), conf)
	})

	t.Run("the same ports change nothing", func(t *testing.T) {
		changed, err := writeIngressFiles(cfg, []int{7000, 8080})
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("no ports, nothing published", func(t *testing.T) {
		changed, err := writeIngressFiles(cfg, nil)
		require.NoError(t, err)
		assert.True(t, changed)
		data, err := ingressComposeFile(cfg).ReadFile()
		require.NoError(t, err)
		assert.NotContains(t, string(data), "ports:")
		assert.Empty(t, loadCompose(t, ingressComposeFile(cfg)).Services[ingressServiceName].Ports)
		assertIngressConf(t, cfg, nil)
	})
}

const testIngressID = "ingress-id"

// fakeDocker answers the container lists with the containers of each label.
type fakeDocker struct {
	dockerClient.APIClient
	byLabel map[string]container.Summary
}

func (f *fakeDocker) ContainerList(_ context.Context, options dockerClient.ContainerListOptions) (dockerClient.ContainerListResult, error) {
	for label := range options.Filters["label"] {
		if c, ok := f.byLabel[strings.TrimSuffix(label, "=true")]; ok {
			return dockerClient.ContainerListResult{Items: []container.Summary{c}}, nil
		}
	}
	return dockerClient.ContainerListResult{}, nil
}

// Ping reports the daemon as unreachable, so that the CLI does not negotiate with the fake.
func (f *fakeDocker) Ping(context.Context, dockerClient.PingOptions) (dockerClient.PingResult, error) {
	return dockerClient.PingResult{}, errors.New("docker daemon not reachable")
}

// fakeDockerCli is a docker CLI on the fake.
func fakeDockerCli(t *testing.T, fake *fakeDocker) command.Cli {
	t.Helper()
	cli, err := command.NewDockerCli(command.WithAPIClient(fake), command.WithBaseContext(t.Context()))
	require.NoError(t, err)
	require.NoError(t, cli.Initialize(&flags.ClientOptions{}))
	return cli
}

func TestApplyIngress(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)

	t.Run("a stopped ingress publishes nothing: closing writes the files only", func(t *testing.T) {
		_, err := writeIngressFiles(cfg, []int{7000})
		require.NoError(t, err)
		stopped := &fakeDocker{byLabel: map[string]container.Summary{DockerIngressLabel: {ID: testIngressID, State: container.StateExited}}}
		require.NoError(t, closeAppPorts(t.Context(), fakeDockerCli(t, stopped), cfg))
		assertIngressConf(t, cfg, nil)
	})

	t.Run("a running ingress with the same ports is left as is", func(t *testing.T) {
		_, err := writeIngressFiles(cfg, []int{7000})
		require.NoError(t, err)
		running := &fakeDocker{byLabel: map[string]container.Summary{DockerIngressLabel: {ID: testIngressID, State: container.StateRunning}}}
		require.NoError(t, applyIngress(t.Context(), fakeDockerCli(t, running), cfg, []int{7000}, false))
	})
}

func TestStopAppProcessesClosesThePortsOfItsApp(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	require.NoError(t, cfg.RuntimeDir().MkdirAll())
	// The launcher is unreachable: it runs nothing, the ingress is stopped
	docker := fakeDockerCli(t, &fakeDocker{byLabel: map[string]container.Summary{DockerIngressLabel: {ID: testIngressID, State: container.StateExited}}})
	stopped := newTestApp(t, cfg, "stopped")
	stopped.MainPythonFile = stopped.FullPath.Join("python", "main.py")

	t.Run("another app keeps its ports", func(t *testing.T) {
		_, err := writeIngressFiles(cfg, []int{7000})
		require.NoError(t, err)
		require.NoError(t, setLauncherWantedApp(cfg, cfg.AppsDir().Join("other")))
		require.NoError(t, stopAppProcesses(t.Context(), docker, stopped, cfg, nil))
		assertIngressConf(t, cfg, []int{7000})
	})

	t.Run("its own ports close", func(t *testing.T) {
		require.NoError(t, setLauncherWantedApp(cfg, stopped.FullPath))
		require.NoError(t, stopAppProcesses(t.Context(), docker, stopped, cfg, nil))
		assertIngressConf(t, cfg, nil)
		assert.Nil(t, readLauncherWantedApp(cfg))
		data, err := ingressComposeFile(cfg).ReadFile()
		require.NoError(t, err)
		assert.NotContains(t, string(data), "ports:")
	})
}

// assertIngressConf checks that the ingress conf forwards the ports.
func assertIngressConf(t *testing.T, cfg config.Configuration, ports []int) {
	t.Helper()
	conf, err := ingressConfFile(cfg).ReadFile()
	require.NoError(t, err)
	assert.Equal(t, string(generateIngressConf(ports)), string(conf))
}
