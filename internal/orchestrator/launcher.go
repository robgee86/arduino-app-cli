// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

const (
	DockerLauncherLabel = "cc.arduino.app.launcher"

	// launcherNetwork is shared by the launcher and the app sidecars, which reach each other by service name.
	launcherNetwork      = "arduino-apps"
	launcherSocketDir    = "/run/arduino-app-cli"
	launcherReadyTimeout = 90 * time.Second

	launcherRequestTimeout = 2 * time.Second
	// A stop waits a 5 s grace before SIGKILL.
	launcherStopTimeout = 60 * time.Second
	// launcherPrepareTimeout bounds the install of the app dependencies, the launcher is given it.
	launcherPrepareTimeout = 600 * time.Second
	// A start stops the running app, prepares the new one and forks it.
	launcherStartTimeout = launcherStopTimeout + launcherPrepareTimeout
)

// appPorts are the public ports of an app: the defaults of its bricks, then the ports of app.yaml.
func appPorts(a app.ArduinoApp, bricksIndex *bricksindex.BricksIndex) []int {
	// A copy: WithAppBricks would change the index shared by the callers
	index := &bricksindex.BricksIndex{BuiltInBricks: bricksIndex.BuiltInBricks, AppBricks: a.LocalBricks}
	var ports []int
	for _, b := range a.Descriptor.Bricks {
		brick, found := index.FindBrickByID(b.ID)
		if !found {
			continue
		}
		for _, p := range brick.Ports {
			port, err := strconv.Atoi(p)
			if err != nil {
				slog.Warn("skipping invalid brick port", slog.String("brick_id", brick.ID), slog.String("port", p))
				continue
			}
			ports = append(ports, port)
		}
	}
	ports = append(ports, a.Descriptor.Ports...)
	slices.Sort(ports)
	return slices.Compact(ports)
}

// checkLauncherCanRun checks that a slot can run the app: its folder is one the CLI mirrors, its ports
// ones the ingress can publish.
func checkLauncherCanRun(appToStart app.ArduinoApp, ports []int, cfg config.Configuration) error {
	if roots := launcherAppRoots(cfg); !isInsideLauncherRoots(appToStart.FullPath, cfg) {
		return fmt.Errorf("the app launcher runs only the apps inside %s", strings.Join(roots.AsStrings(), " or "))
	}
	const maxPort = 65535
	for _, p := range ports {
		if p < 1 || p > maxPort {
			return fmt.Errorf("the ingress cannot publish port %d: the app ports go from 1 to %d", p, maxPort)
		}
	}
	return nil
}

// launcherAppRoots are the folders whose apps the slots run, each mirrored on its own.
func launcherAppRoots(cfg config.Configuration) paths.PathList {
	return paths.PathList{cfg.AppsDir(), cfg.ExamplesBaseDir(), cfg.ReleasesDir()}
}

func isInsideLauncherRoots(appPath *paths.Path, cfg config.Configuration) bool {
	for _, root := range launcherAppRoots(cfg) {
		if inside, err := appPath.IsInsideDir(root); err == nil && inside {
			return true
		}
	}
	return false
}

// writeFileIfChanged writes data atomically unless the file already holds it, and tells whether it wrote.
func writeFileIfChanged(file *paths.Path, data []byte) (bool, error) {
	if before, err := file.ReadFile(); err == nil && bytes.Equal(before, data) {
		return false, nil
	}
	if err := file.Parent().MkdirAll(); err != nil {
		return false, err
	}
	if err := fatomic.WriteFile(file.String(), data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// boardEnvironment is what the launcher container is given: the host facts that hold for every app.
// The camera is a fact of the app start: a plugged camera must not recreate the launcher.
func boardEnvironment(ctx context.Context, plat platform.Platform, cfg config.Configuration) types.Mapping {
	envs := hostEnvironment(ctx, cfg.AppsDir(), cfg)
	delete(envs, appHomeVar)
	delete(envs, "VIDEO_DEVICE")
	envs["BOARD_NAME"] = plat.BoardName
	return envs
}

// waitLauncherReady polls ping until the preload is over. It fails early when the container stops
// or restarts, as an image without the serve mode does.
func waitLauncherReady(ctx context.Context, docker dockerClient.APIClient, client *launcher.Client, id string) error {
	const pingTimeout = 500 * time.Millisecond
	inspected, err := docker.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("failed to inspect the app launcher: %w", err)
	}
	restarts := inspected.Container.RestartCount
	client = client.WithTimeout(pingTimeout)
	deadline := time.Now().Add(launcherReadyTimeout)
	for {
		if err := client.Ping(ctx); err == nil {
			return nil
		}
		inspected, err := docker.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("failed to inspect the app launcher: %w", err)
		}
		info := inspected.Container
		if info.State == nil || !info.State.Running || info.State.Restarting || info.RestartCount > restarts {
			return fmt.Errorf("the app launcher container stopped, see `docker logs %s`", strings.TrimPrefix(info.Name, "/"))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the app launcher did not answer within %s", launcherReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// getContainerWithLabel returns the container carrying the label, running or not, nil without one.
func getContainerWithLabel(ctx context.Context, docker dockerClient.APIClient, label string) (*container.Summary, error) {
	containers, err := dockerhelper.Containers(ctx, docker, label+"=true")
	if err != nil {
		return nil, fmt.Errorf("failed to list the %s container: %w", label, err)
	}
	if len(containers) == 0 {
		return nil, nil
	}
	return &containers[0], nil
}

// stopContainerWithLabel stops the container carrying the label, which publishes ports even in a restart loop.
func stopContainerWithLabel(ctx context.Context, docker dockerClient.APIClient, label string) error {
	current, err := getContainerWithLabel(ctx, docker, label)
	if err != nil || current == nil || (current.State != container.StateRunning && current.State != container.StateRestarting) {
		return err
	}
	slog.Debug("stopping container", slog.String("label", label), slog.String("id", current.ID))
	if _, err := docker.ContainerStop(ctx, current.ID, dockerClient.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("failed to stop %s: %w", containerName(*current), err)
	}
	return nil
}

// StopAllAppContainers stops every running app container, sidecars included, leaving the launcher
// and the ingress up: the ingress carries no app label.
func StopAllAppContainers(ctx context.Context, docker dockerClient.APIClient) error {
	containers, err := docker.ContainerList(ctx, dockerClient.ContainerListOptions{
		Filters: make(dockerClient.Filters).Add("label", DockerAppLabel+"=true"),
	})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}
	for _, c := range containers.Items {
		if c.Labels[DockerLauncherLabel] == "true" {
			continue
		}
		slog.Debug("Stopping container", slog.String("ID", c.ID))
		if _, err := docker.ContainerStop(ctx, c.ID, dockerClient.ContainerStopOptions{}); err != nil {
			slog.Warn("Failed to stop container", "ID", c.ID, "error", err.Error())
		}
	}
	return nil
}

// composeUpFile creates, recreates or starts the containers of one of our compose files, as
// compose up does, and returns once they are started.
func composeUpFile(ctx context.Context, docker command.Cli, file *paths.Path, cb func(StreamMessage)) error {
	prj, err := loader.LoadWithContext(ctx,
		types.ConfigDetails{
			ConfigFiles: []types.ConfigFile{{Filename: file.String()}},
			WorkingDir:  file.Parent().String(),
			Environment: types.Mapping{},
		},
		func(o *loader.Options) { o.ResolvePaths = true },
	)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", file, err)
	}
	prj.ComposeFiles = []string{file.String()}
	return dockerhelper.ComposeUp(ctx, docker, prj, func(line string) { cb(StreamMessage{data: line}) })
}

// EnsureLauncher prepares the slots, as the daemon does at boot: it clears what a previous run left,
// the apps it ran included, warms a spare and pulls the ingress image, so that no start has to.
func EnsureLauncher(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	platform platform.Platform,
	_ bool,
) (time.Duration, error) {
	started := time.Now()
	err := withAppsLock(ctx, cfg, func() error {
		clearSlots(ctx, docker, cfg)
		if err := errors.Join(ensureAppsNetwork(ctx, docker.Client()), closeAppPorts(ctx, docker.Client())); err != nil {
			return err
		}
		if err := pullIngressImage(ctx, docker.Client(), func(StreamMessage) {}); err != nil {
			slog.Warn("the ingress image is unavailable, the apps with ports cannot start", slog.String("error", err.Error()))
		}
		ensureSpare(ctx, docker, cfg, platform)
		return nil
	})
	return time.Since(started), err
}
