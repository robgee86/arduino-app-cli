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
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	yaml "github.com/goccy/go-yaml"
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

	launcherProject     = "arduino-app-launcher"
	launcherServiceName = "launcher"
	// launcherNetwork is shared by the launcher and the app sidecars, which reach each other by service name.
	launcherNetwork      = "arduino-apps"
	launcherSocketDir    = "/run/arduino-app-cli"
	defaultLauncherPort  = 7000
	launcherReadyTimeout = 90 * time.Second

	launcherRequestTimeout = 2 * time.Second
	// A stop waits a 5 s grace before SIGKILL.
	launcherStopTimeout = 60 * time.Second
	// launcherPrepareTimeout bounds the install of the app dependencies, the launcher is given it.
	launcherPrepareTimeout = 600 * time.Second
	// A start stops the running app, prepares the new one and forks it.
	launcherStartTimeout = launcherStopTimeout + launcherPrepareTimeout
)

func launcherComposeFile(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("launcher", "compose.yaml")
}

// launcherWantedAppFile holds the app the launcher should be running: a missing child means the app died.
func launcherWantedAppFile(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("launcher", "wanted-app")
}

func newLauncherClient(cfg config.Configuration) *launcher.Client {
	return launcher.New(cfg.RuntimeDir().Join(launcher.SocketName).String(), launcherRequestTimeout)
}

// launcherPorts are the public ports the launcher forwards to the running app: the brick ports.
func launcherPorts(bricksIndex *bricksindex.BricksIndex) []int {
	ports := []int{defaultLauncherPort}
	for _, brick := range bricksIndex.ListBricks() {
		for _, p := range brick.Ports {
			port, err := strconv.Atoi(p)
			if err != nil {
				slog.Warn("skipping invalid brick port", slog.String("brick_id", brick.ID), slog.String("port", p))
				continue
			}
			ports = append(ports, port)
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

// prepareLauncher checks that the launcher can run the app, then brings it up. bricksIndex holds
// the installed bricks only: their ports are the ones the launcher forwards.
func prepareLauncher(
	ctx context.Context,
	docker command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	appToStart app.ArduinoApp,
	cfg config.Configuration,
	platform platform.Platform,
	cb func(StreamMessage),
) error {
	if port, ok := uncoveredAppPort(appToStart, bricksIndex); ok {
		return fmt.Errorf("the app launcher does not forward port %d declared in app.yaml: only the ports of the installed bricks are forwarded", port)
	}
	if roots := launcherAppRoots(cfg); !isInsideLauncherRoots(appToStart.FullPath, cfg) {
		return fmt.Errorf("the app launcher runs only the apps inside %s", strings.Join(roots.AsStrings(), " or "))
	}
	elapsed, err := ensureLauncher(ctx, docker, bricksIndex, cfg, platform, cb)
	if err != nil {
		return fmt.Errorf("the app launcher is unavailable: %w", err)
	}
	cb(StreamMessage{data: fmt.Sprintf("App launcher ready in %s", elapsed.Round(time.Millisecond))})
	return nil
}

// uncoveredAppPort returns an app.yaml port the launcher does not publish.
func uncoveredAppPort(a app.ArduinoApp, bricksIndex *bricksindex.BricksIndex) (int, bool) {
	ports := launcherPorts(bricksIndex)
	for _, p := range a.Descriptor.Ports {
		if !slices.Contains(ports, p) {
			return p, true
		}
	}
	return 0, false
}

// launcherAppRoots are the folders mounted in the launcher at their host path: the apps run in place.
func launcherAppRoots(cfg config.Configuration) paths.PathList {
	return paths.PathList{cfg.AppsDir(), cfg.ExamplesBaseDir()}
}

func isInsideLauncherRoots(appPath *paths.Path, cfg config.Configuration) bool {
	for _, root := range launcherAppRoots(cfg) {
		if inside, err := appPath.IsInsideDir(root); err == nil && inside {
			return true
		}
	}
	return false
}

// generateLauncherComposeFile writes the launcher compose project, rendered on this board, and
// tells whether it changed.
func generateLauncherComposeFile(
	bricksIndex *bricksindex.BricksIndex,
	pythonImage string,
	cfg config.Configuration,
	boardEnvs types.Mapping,
	platform platform.Platform,
) (bool, error) {
	ports := launcherPorts(bricksIndex)
	published := make([]string, 0, len(ports))
	forwarded := make([]string, 0, len(ports))
	for _, p := range ports {
		published = append(published, fmt.Sprintf("%d:%d", p, p))
		forwarded = append(forwarded, strconv.Itoa(p))
	}

	envs := boardEnvs.Clone()
	envs["APP_LAUNCHER_PORTS"] = strings.Join(forwarded, ",")
	envs["APP_LAUNCHER_SOCKET"] = launcherSocketDir + "/" + launcher.SocketName
	envs["APP_LAUNCHER_PREPARE_TIMEOUT_S"] = strconv.Itoa(int(launcherPrepareTimeout.Seconds()))

	// The launcher is given host paths
	volumes := []any{volume{Type: "bind", Source: cfg.RuntimeDir().String(), Target: launcherSocketDir}}
	for _, root := range launcherAppRoots(cfg) {
		expr, err := mountExpr(root.String())
		if err != nil {
			return false, err
		}
		volumes = append(volumes, expr)
	}
	runtime, err := newPythonRuntime(cfg, platform)
	if err != nil {
		return false, err
	}
	svc := runtime.service(pythonImage, volumes...)
	svc.Ports = published
	svc.Entrypoint = "/run.sh serve"
	svc.Restart = "unless-stopped"
	svc.StopGracePeriod = "10s"
	svc.Healthcheck = &healthcheck{
		Test:          []string{"CMD", "arduino-app-launcher", "ping"},
		Interval:      "2s",
		StartPeriod:   "60s",
		StartInterval: "500ms",
	}
	svc.Labels = map[string]string{DockerLauncherLabel: "true"}
	svc.Environment = envs

	compose := struct {
		Name     string                    `yaml:"name"`
		Services map[string]service        `yaml:"services"`
		Networks map[string]composeNetwork `yaml:"networks"`
	}{
		Name:     launcherProject,
		Services: map[string]service{launcherServiceName: svc},
		Networks: map[string]composeNetwork{"default": {Name: launcherNetwork}},
	}
	template, err := yaml.Marshal(compose)
	if err != nil {
		return false, err
	}
	// The host devices and groups are answered here, the board the launcher runs on
	data, err := renderComposeTemplate(template)
	if err != nil {
		return false, fmt.Errorf("failed to render the launcher compose file: %w", err)
	}

	file := launcherComposeFile(cfg)
	if before, err := file.ReadFile(); err == nil && bytes.Equal(before, data) {
		return false, nil
	}
	if err := file.Parent().MkdirAll(); err != nil {
		return false, fmt.Errorf("failed to create the launcher folder: %w", err)
	}
	if err := fatomic.WriteFile(file.String(), data, 0o644); err != nil {
		return false, fmt.Errorf("failed to write the launcher compose file: %w", err)
	}
	return true, nil
}

// boardEnvironment is what the launcher container is given: the host facts that hold for every app.
// The camera is a fact of the app start: a plugged camera must not recreate the launcher.
func boardEnvironment(ctx context.Context, plat platform.Platform, cfg config.Configuration) types.Mapping {
	envs := hostEnvironment(ctx, cfg.AppsDir(), cfg)
	delete(envs, "APP_HOME")
	delete(envs, "VIDEO_DEVICE")
	envs["BOARD_NAME"] = plat.BoardName
	return envs
}

// ensureLauncher brings the launcher up to date and running, and waits until it answers.
func ensureLauncher(
	ctx context.Context,
	docker command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	cfg config.Configuration,
	platform platform.Platform,
	cb func(StreamMessage),
) (time.Duration, error) {
	started := time.Now()
	if err := cfg.RuntimeDir().MkdirAll(); err != nil {
		return 0, fmt.Errorf("failed to create the runtime folder: %w", err)
	}
	changed, err := generateLauncherComposeFile(bricksIndex, cfg.PythonImage, cfg, boardEnvironment(ctx, platform, cfg), platform)
	if err != nil {
		return 0, err
	}

	current, err := getLauncherContainer(ctx, docker.Client())
	if err != nil {
		return 0, err
	}
	if changed || current == nil || current.State != container.StateRunning {
		slog.Debug("bringing the app launcher up", slog.Bool("changed", changed), slog.Bool("exists", current != nil))
		cb(StreamMessage{data: "Starting the app launcher"})
		if err := composeUpFile(ctx, docker, launcherComposeFile(cfg), cb); err != nil {
			return 0, fmt.Errorf("failed to start the app launcher: %w", err)
		}
		// A new or restarted launcher runs no app
		if err := clearLauncherWantedApp(cfg, nil); err != nil {
			return 0, err
		}
		if current, err = getLauncherContainer(ctx, docker.Client()); err != nil {
			return 0, err
		}
		if current == nil {
			return 0, errors.New("the app launcher container was not created")
		}
	}

	if err := waitLauncherReady(ctx, docker.Client(), newLauncherClient(cfg), current.ID); err != nil {
		return 0, err
	}
	elapsed := time.Since(started)
	slog.Info("app launcher ready", slog.Duration("elapsed", elapsed), slog.Bool("recreated", changed))
	return elapsed, nil
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

func getLauncherContainer(ctx context.Context, docker dockerClient.APIClient) (*container.Summary, error) {
	containers, err := dockerhelper.Containers(ctx, docker, DockerLauncherLabel+"=true")
	if err != nil {
		return nil, fmt.Errorf("failed to list the app launcher container: %w", err)
	}
	if len(containers) == 0 {
		return nil, nil
	}
	return &containers[0], nil
}

func stopLauncherContainer(ctx context.Context, docker dockerClient.APIClient) error {
	current, err := getLauncherContainer(ctx, docker)
	// A launcher in a restart loop grabs the ports at each attempt
	if err != nil || current == nil || (current.State != container.StateRunning && current.State != container.StateRestarting) {
		return err
	}
	slog.Debug("stopping the app launcher", slog.String("id", current.ID))
	if _, err := docker.ContainerStop(ctx, current.ID, dockerClient.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("failed to stop the app launcher: %w", err)
	}
	return nil
}

// StopAllAppContainers stops every running app container, sidecars included, leaving the launcher up.
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

func readLauncherWantedApp(cfg config.Configuration) *paths.Path {
	data, err := launcherWantedAppFile(cfg).ReadFile()
	if err != nil || len(data) == 0 {
		return nil
	}
	return paths.New(string(data))
}

func setLauncherWantedApp(cfg config.Configuration, appPath *paths.Path) error {
	file := launcherWantedAppFile(cfg)
	if err := file.Parent().MkdirAll(); err != nil {
		return err
	}
	return fatomic.WriteFile(file.String(), []byte(appPath.String()), 0o644)
}

// clearLauncherWantedApp forgets the wanted app when it is appPath, or whatever it is when nil.
func clearLauncherWantedApp(cfg config.Configuration, appPath *paths.Path) error {
	if wanted := readLauncherWantedApp(cfg); wanted == nil || (appPath != nil && !wanted.EqualsTo(appPath)) {
		return nil
	}
	if err := launcherWantedAppFile(cfg).Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// EnsureLauncher brings the launcher up and stops what it runs, as the daemon does at boot.
func EnsureLauncher(
	ctx context.Context,
	docker command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	cfg config.Configuration,
	platform platform.Platform,
	stopApps bool,
) (time.Duration, error) {
	var elapsed time.Duration
	err := withAppsLock(ctx, cfg, func() error {
		var err error
		if elapsed, err = ensureLauncher(ctx, docker, bricksIndex, cfg, platform, func(StreamMessage) {}); err != nil {
			return err
		}
		if !stopApps {
			return nil
		}
		return stopLauncherApp(ctx, cfg, nil)
	})
	return elapsed, err
}

// LauncherDown removes the launcher container and its network, stopping every app first.
func LauncherDown(ctx context.Context, docker command.Cli, cfg config.Configuration, cb func(StreamMessage)) error {
	return withAppsLock(ctx, cfg, func() error {
		// The network cannot go while the sidecars are attached to it
		if err := StopAllAppContainers(ctx, docker.Client()); err != nil {
			return err
		}
		if err := clearLauncherWantedApp(cfg, nil); err != nil {
			return err
		}
		return dockerhelper.ComposeDown(ctx, docker, launcherProject, func(line string) { cb(StreamMessage{data: line}) })
	})
}

// LauncherStatus is what the launcher runs, as it reports it.
func LauncherStatus(ctx context.Context, cfg config.Configuration) (launcher.StatusResponse, error) {
	return newLauncherClient(cfg).Status(ctx)
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
