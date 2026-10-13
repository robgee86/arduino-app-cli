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

	"github.com/arduino/go-paths-helper"
	"github.com/docker/cli/cli/command"
	yaml "github.com/goccy/go-yaml"
	"github.com/moby/moby/api/types/container"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

// The ingress is the only container publishing app ports: an nginx on the network of the launcher
// that publishes the public ports of the running app and forwards each to its internal port in the
// launcher, and publishes nothing without app. Docker fixes the published ports at container
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

// generateIngressConf forwards each public port to its internal port in the launcher. nginx finds
// the launcher by its service name through the Docker DNS at each connection, so that it follows a
// recreated launcher and starts without one.
func generateIngressConf(ports []int) []byte {
	var b strings.Builder
	b.WriteString("worker_processes 1;\nerror_log /dev/stderr warn;\npid /tmp/nginx.pid;\nevents {}\n")
	if len(ports) == 0 {
		b.WriteString("stream {}\n")
		return []byte(b.String())
	}
	b.WriteString("stream {\n    resolver 127.0.0.11 valid=5s ipv6=off;\n")
	for _, p := range ports {
		fmt.Fprintf(&b, "    server {\n        listen %d;\n        set $launcher %s;\n        proxy_pass $launcher:%d;\n    }\n", p, launcherServiceName, launcher.InternalPort(p))
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
		// The ports are the configuration: the conf is never changed under a running ingress
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

// writeIngressFiles writes the ingress for the ports and tells whether its compose file changed.
// The conf goes first: a container created from the compose file finds it.
func writeIngressFiles(cfg config.Configuration, ports []int) (bool, error) {
	ports = slices.Compact(slices.Sorted(slices.Values(ports)))
	if _, err := writeFileIfChanged(ingressConfFile(cfg), generateIngressConf(ports)); err != nil {
		return false, fmt.Errorf("failed to write the ingress configuration: %w", err)
	}
	data, err := generateIngressComposeFile(cfg, ports)
	if err != nil {
		return false, err
	}
	changed, err := writeFileIfChanged(ingressComposeFile(cfg), data)
	if err != nil {
		return false, fmt.Errorf("failed to write the ingress compose file: %w", err)
	}
	return changed, nil
}

// applyIngress makes the ingress publish exactly the ports, none when empty. compose up recreates
// it only when the ports changed or it does not run; forceUp also follows a new image. A stopped
// ingress publishes nothing, so it is left so when there is nothing to publish.
func applyIngress(ctx context.Context, docker command.Cli, cfg config.Configuration, ports []int, forceUp bool) error {
	changed, err := writeIngressFiles(cfg, ports)
	if err != nil {
		return err
	}
	current, err := getContainerWithLabel(ctx, docker.Client(), DockerIngressLabel)
	if err != nil {
		return err
	}
	running := current != nil && current.State == container.StateRunning
	// A restart loop binds the ports at each attempt
	publishing := running || (current != nil && current.State == container.StateRestarting)
	if len(ports) == 0 && !publishing && !forceUp {
		slog.Debug("ingress not running, no port to close")
		return nil
	}
	reason, up := composeUpReason(forceUp, changed, running)
	if !up {
		slog.Debug("ingress publishes the ports already", slog.Any("ports", ports))
		return nil
	}
	if err := upIngress(ctx, docker, cfg, reason); err != nil {
		return err
	}
	slog.Info("ingress publishes the app ports", slog.Any("ports", ports), slog.String("reason", reason))
	return nil
}

// ensureIngress brings the ingress up on the ports it publishes, none on a first run, following a new image.
func ensureIngress(ctx context.Context, docker command.Cli, cfg config.Configuration) error {
	if ingressComposeFile(cfg).NotExist() || ingressConfFile(cfg).NotExist() {
		return applyIngress(ctx, docker, cfg, nil, true)
	}
	return upIngress(ctx, docker, cfg, "forced, to follow the image")
}

// upIngress runs compose up on the ingress and returns once it runs.
func upIngress(ctx context.Context, docker command.Cli, cfg config.Configuration, reason string) error {
	slog.Debug("running compose up for the ingress", slog.String("reason", reason))
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
	if err := waitContainerRunning(ctx, docker.Client(), current.ID); err != nil {
		return fmt.Errorf("the ingress is not running, see `docker logs %s`: %w", containerName(*current), err)
	}
	return nil
}

// closeAppPorts makes the ingress publish nothing.
func closeAppPorts(ctx context.Context, docker command.Cli, cfg config.Configuration) error {
	if err := applyIngress(ctx, docker, cfg, nil, false); err != nil {
		return fmt.Errorf("failed to close the app ports: %w", err)
	}
	return nil
}
