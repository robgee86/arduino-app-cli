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
	"strconv"
	"strings"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/docker/cli/cli/command"
	yaml "github.com/goccy/go-yaml"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// The ingress is the only container publishing app ports: an nginx on the apps network that
// publishes the ports of every app of the board and forwards the ones of the running app to its
// container. Docker fixes the published ports at container creation, so the published set only
// grows while the daemon runs and a switch between apps is a reload of the conf. The daemon boot
// computes the set again.
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

// generateIngressConf listens on the ports and forwards each to the same port of the host, where
// the app binds it. nginx resolves the host through the Docker DNS at each connection, so that it
// follows a recreated container and starts without one.
func generateIngressConf(host string, ports []int) []byte {
	var b strings.Builder
	b.WriteString("worker_processes 1;\nerror_log /dev/stderr warn;\npid /tmp/nginx.pid;\nevents {}\n")
	if len(ports) == 0 {
		b.WriteString("stream {}\n")
		return []byte(b.String())
	}
	b.WriteString("stream {\n    resolver 127.0.0.11 valid=5s ipv6=off;\n")
	for _, p := range ports {
		fmt.Fprintf(&b, "    server {\n        listen %d;\n        set $app %s;\n        proxy_pass $app:%d;\n    }\n", p, host, p)
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
		// nginx reads the conf as it starts and at each SIGHUP
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
		Networks: map[string]composeNetwork{"default": {Name: appsNetwork, External: true}},
	}
	return yaml.Marshal(compose)
}

// publishedIngressPorts are the ports the ingress compose file publishes, none without one.
func publishedIngressPorts(cfg config.Configuration) []int {
	data, err := ingressComposeFile(cfg).ReadFile()
	if err != nil {
		return nil
	}
	var compose struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		slog.Warn("the ingress compose file is unreadable, its ports are published again", slog.String("error", err.Error()))
		return nil
	}
	var ports []int
	for _, mapping := range compose.Services[ingressServiceName].Ports {
		published, _, _ := strings.Cut(mapping, ":")
		if port, err := strconv.Atoi(published); err == nil {
			ports = append(ports, port)
		}
	}
	return ports
}

// ingressFiles tells which of the ingress files a write changed.
type ingressFiles struct {
	Conf    bool
	Compose bool
}

// writeIngressFiles writes the ingress publishing the ports and listening on the ones of the host,
// and tells which files changed. The conf goes first: a container created from the compose file finds it.
func writeIngressFiles(cfg config.Configuration, published []int, host string, listen []int) (ingressFiles, error) {
	confChanged, err := writeFileIfChanged(ingressConfFile(cfg), generateIngressConf(host, sortedPorts(listen)))
	if err != nil {
		return ingressFiles{}, fmt.Errorf("failed to write the ingress configuration: %w", err)
	}
	data, err := generateIngressComposeFile(cfg, sortedPorts(published))
	if err != nil {
		return ingressFiles{}, err
	}
	composeChanged, err := writeFileIfChanged(ingressComposeFile(cfg), data)
	if err != nil {
		return ingressFiles{}, fmt.Errorf("failed to write the ingress compose file: %w", err)
	}
	return ingressFiles{Conf: confChanged, Compose: composeChanged}, nil
}

func sortedPorts(ports ...[]int) []int {
	return slices.Compact(slices.Sorted(slices.Values(slices.Concat(ports...))))
}

// routeIngress makes the ingress forward the ports of the app to its container. A port the ingress
// does not publish yet grows the published set with the ports of every app of the board.
func routeIngress(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	bricksIndex *bricksindex.BricksIndex,
	c appContainer,
	ports []int,
	cb func(StreamMessage),
) error {
	published := publishedIngressPorts(cfg)
	if slices.ContainsFunc(ports, func(p int) bool { return !slices.Contains(published, p) }) {
		published = sortedPorts(published, boardAppPorts(cfg, bricksIndex), ports)
		slog.Debug("the ingress publishes more ports", slog.Any("ports", published))
	}
	return applyIngress(ctx, docker, cfg, published, c.host(), ports, cb)
}

// syncIngress makes the ingress publish exactly the ports and listen on none, as at boot. It stops
// the ingress without ports.
func syncIngress(ctx context.Context, docker command.Cli, cfg config.Configuration, published []int) error {
	if len(published) > 0 {
		// Pulled here so that no start has to
		if err := pullIngressImage(ctx, docker.Client(), func(StreamMessage) {}); err != nil {
			return err
		}
	}
	if err := applyIngress(ctx, docker, cfg, published, "", nil, func(StreamMessage) {}); err != nil {
		return err
	}
	if len(published) == 0 {
		return stopIngress(ctx, docker.Client())
	}
	return nil
}

// applyIngress writes the ingress files and brings the container to them: a new compose file
// recreates it, a new conf reloads it. Nothing publishes without ports.
func applyIngress(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	published []int,
	host string,
	listen []int,
	cb func(StreamMessage),
) error {
	changed, err := writeIngressFiles(cfg, published, host, listen)
	if err != nil || len(published) == 0 {
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
		slog.Debug("the ingress forwards the ports already", slog.Any("ports", listen))
		return nil
	case current.State == container.StateRunning:
		if err := reloadIngress(ctx, docker.Client(), *current); err != nil {
			return err
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
	slog.Info("ingress forwards the app ports", slog.Any("ports", listen), slog.String("host", host), slog.Bool("recreated", changed.Compose || current == nil))
	return nil
}

// silenceIngress makes the ingress listen on no port. It keeps publishing them, and stays up.
func silenceIngress(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration) error {
	changed, err := writeFileIfChanged(ingressConfFile(cfg), generateIngressConf("", nil))
	if err != nil || !changed {
		return err
	}
	current, err := getContainerWithLabel(ctx, docker, DockerIngressLabel)
	if err != nil || current == nil || current.State != container.StateRunning {
		return err
	}
	return reloadIngress(ctx, docker, *current)
}

// reloadIngress has nginx read its conf again, in place.
func reloadIngress(ctx context.Context, docker dockerClient.APIClient, current container.Summary) error {
	slog.Debug("reloading the ingress on its new configuration")
	if _, err := docker.ContainerKill(ctx, current.ID, dockerClient.ContainerKillOptions{Signal: "HUP"}); err != nil {
		return fmt.Errorf("failed to reload the ingress: %w", err)
	}
	return nil
}

// stopIngress stops the ingress, which closes the ports it publishes, a restart loop included.
func stopIngress(ctx context.Context, docker dockerClient.APIClient) error {
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
