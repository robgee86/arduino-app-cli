// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/docker/cli/cli/command"
	yaml "github.com/goccy/go-yaml"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// The ingress is the only container publishing app ports: an nginx on the network of the launcher
// that publishes the public ports of the running app and forwards each to its internal port in the
// launcher. It runs only while an app with ports runs. Docker fixes the published ports at container
// creation, so compose recreates the ingress when the ports change.
const (
	DockerIngressLabel = "cc.arduino.app.ingress"
	// IngressImage ships the nginx stream module.
	IngressImage = "nginx:1.27-alpine"

	ingressProject     = "arduino-ingress"
	ingressServiceName = "ingress"
	ingressConfTarget  = "/etc/nginx"
)

func ingressComposeFile(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("ingress", "compose.yaml")
}

// ingressConfFile is alone in a mounted folder: a file mount would keep the inode an atomic write replaces.
func ingressConfFile(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("ingress", "nginx", "nginx.conf")
}

// generateIngressConf forwards each public port to the same port in the slot running the app, upstream.
// nginx finds the slot by its service name through the Docker DNS at each connection, so that it
// starts without one.
func generateIngressConf(ports []int, upstream string) []byte {
	var b strings.Builder
	b.WriteString("worker_processes 1;\nerror_log /dev/stderr warn;\npid /tmp/nginx.pid;\nevents {}\n")
	if len(ports) == 0 {
		b.WriteString("stream {}\n")
		return []byte(b.String())
	}
	b.WriteString("stream {\n    resolver 127.0.0.11 valid=5s ipv6=off;\n")
	for _, p := range ports {
		fmt.Fprintf(&b, "    server {\n        listen %d;\n        set $launcher %s;\n        proxy_pass $launcher:%d;\n    }\n", p, upstream, p)
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

// generateIngressComposeFile is the ingress compose project publishing the ports. The container is
// read-only, without capabilities, and nginx runs as its own user: Docker lets any user bind any
// port inside a container.
func generateIngressComposeFile(cfg config.Configuration, ports []int) ([]byte, error) {
	published := make([]string, 0, len(ports))
	for _, p := range ports {
		published = append(published, fmt.Sprintf("%d:%d", p, p))
	}
	svc := service{
		Image: IngressImage,
		Init:  new(true),
		User:  "nginx",
		Ports: published,
		// nginx reads the conf as it starts, which a change of the ports always is
		Volumes:  []any{volume{Type: "bind", Source: ingressConfFile(cfg).Parent().String(), Target: ingressConfTarget, ReadOnly: true}},
		ReadOnly: true,
		// The only file nginx writes, its pid, goes there
		Tmpfs:       []string{"/tmp"},
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges:true"},
		Restart:     "unless-stopped",
		// A fast shutdown: the graceful one waits for the open connections, as a web UI keeps
		StopSignal: "SIGTERM",
		Labels:     map[string]string{DockerIngressLabel: "true"},
		Logging:    containerLogging(),
	}
	compose := struct {
		Name     string                    `yaml:"name"`
		Services map[string]service        `yaml:"services"`
		Networks map[string]composeNetwork `yaml:"networks"`
	}{
		Name:     ingressProject,
		Services: map[string]service{ingressServiceName: svc},
		Networks: map[string]composeNetwork{"default": {Name: launcherNetwork, External: true}},
	}
	return yaml.Marshal(compose)
}

// ingressFiles tells which of the ingress files a write changed.
type ingressFiles struct {
	Conf    bool
	Compose bool
}

// writeIngressFiles writes the ingress for the ports and tells which files changed.
// The conf goes first: a container created from the compose file finds it.
func writeIngressFiles(cfg config.Configuration, ports []int, upstream string) (ingressFiles, error) {
	ports = slices.Compact(slices.Sorted(slices.Values(ports)))
	confChanged, err := writeFileIfChanged(ingressConfFile(cfg), generateIngressConf(ports, upstream))
	if err != nil {
		return ingressFiles{}, fmt.Errorf("failed to write the ingress configuration: %w", err)
	}
	data, err := generateIngressComposeFile(cfg, ports)
	if err != nil {
		return ingressFiles{}, err
	}
	composeChanged, err := writeFileIfChanged(ingressComposeFile(cfg), data)
	if err != nil {
		return ingressFiles{}, fmt.Errorf("failed to write the ingress compose file: %w", err)
	}
	return ingressFiles{Conf: confChanged, Compose: composeChanged}, nil
}

// openAppPorts makes the ingress publish exactly the ports of the app being started. The ingress
// keeps the last ports while stopped: the same ports start it again, other ones recreate it.
func openAppPorts(ctx context.Context, docker command.Cli, cfg config.Configuration, ports []int, upstream string, cb func(StreamMessage)) error {
	if len(ports) == 0 {
		return closeAppPorts(ctx, docker.Client())
	}
	changed, err := writeIngressFiles(cfg, ports, upstream)
	if err != nil {
		return err
	}
	current, err := getContainerWithLabel(ctx, docker.Client(), DockerIngressLabel)
	if err != nil {
		return err
	}
	switch {
	case changed.Compose || current == nil:
		if err := pullIngressImage(ctx, docker.Client(), cb); err != nil {
			return err
		}
		if err := upIngress(ctx, docker, cfg); err != nil {
			return err
		}
	case current.State == container.StateRunning && !changed.Conf:
		slog.Debug("ingress publishes the ports already", slog.Any("ports", ports))
		return nil
	case current.State == container.StateRunning:
		// nginx reads the conf as it starts: a conf that changed under a running ingress is reloaded in place
		slog.Debug("reloading the ingress on its new configuration", slog.Any("ports", ports))
		if _, err := docker.Client().ContainerKill(ctx, current.ID, dockerClient.ContainerKillOptions{Signal: "HUP"}); err != nil {
			return fmt.Errorf("failed to reload the ingress: %w", err)
		}
	default:
		slog.Debug("starting the stopped ingress on the same ports", slog.String("state", string(current.State)))
		if _, err := docker.Client().ContainerStart(ctx, current.ID, dockerClient.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("failed to start the ingress: %w", err)
		}
		if err := waitIngressRunning(ctx, docker.Client(), *current); err != nil {
			return err
		}
	}
	slog.Info("ingress publishes the app ports", slog.Any("ports", ports), slog.Bool("recreated", changed.Compose || current == nil))
	return nil
}

// closeAppPorts stops the ingress, which closes the ports it publishes, a restart loop included.
func closeAppPorts(ctx context.Context, docker dockerClient.APIClient) error {
	if err := stopContainerWithLabel(ctx, docker, DockerIngressLabel); err != nil {
		return fmt.Errorf("failed to close the app ports: %w", err)
	}
	return nil
}

// pullIngressImage pulls the ingress image when it is missing: system init and the daemon boot
// pull it, so that a start does not have to.
func pullIngressImage(ctx context.Context, docker dockerClient.APIClient, cb func(StreamMessage)) error {
	started := time.Now()
	if err := dockerhelper.PullImages(ctx, docker, []string{IngressImage}, func(line string) { cb(StreamMessage{data: line}) }, nil); err != nil {
		return fmt.Errorf("failed to pull the ingress image: %w", err)
	}
	slog.Debug("ingress image available", slog.String("image", IngressImage), slog.Duration("elapsed", time.Since(started)))
	return nil
}

// upIngress runs compose up on the ingress, which recreates it on new ports, and returns once it runs.
func upIngress(ctx context.Context, docker command.Cli, cfg config.Configuration) error {
	if err := composeUpFile(ctx, docker, ingressComposeFile(cfg), func(StreamMessage) {}); err != nil {
		return fmt.Errorf("failed to start the ingress: %w", err)
	}
	current, err := getContainerWithLabel(ctx, docker.Client(), DockerIngressLabel)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("the ingress container was not created")
	}
	return waitIngressRunning(ctx, docker.Client(), *current)
}

func waitIngressRunning(ctx context.Context, docker dockerClient.APIClient, c container.Summary) error {
	if err := waitContainerRunning(ctx, docker, c.ID); err != nil {
		return fmt.Errorf("the ingress is not running, see `docker logs %s`: %w", containerName(c), err)
	}
	return nil
}
