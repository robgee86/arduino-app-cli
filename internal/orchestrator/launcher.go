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

// maxPort is the last TCP port, the app ports go from 1 to it.
const maxPort = 65535

// launcherPorts are the public ports the launcher publishes: the sorted union of the ports of the
// apps on the board, so that no app start has to recreate it. An app that fails to load or a port
// that is no TCP port is left out: it fails its own start, not the launcher.
func launcherPorts(cfg config.Configuration, bricksIndex *bricksindex.BricksIndex) []int {
	appPaths, err := app.FindAppsInFolders(launcherAppRoots(cfg))
	if err != nil {
		slog.Warn("some apps were not listed for the launcher ports", slog.String("error", err.Error()))
	}
	var ports []int
	for _, appPath := range appPaths {
		a, err := app.Load(appPath)
		if err != nil {
			slog.Debug("skipping the ports of an invalid app", slog.String("path", appPath.String()), slog.String("error", err.Error()))
			continue
		}
		for _, p := range appPorts(a, bricksIndex) {
			if p < 1 || p > maxPort {
				slog.Debug("skipping an invalid app port", slog.String("path", appPath.String()), slog.Int("port", p))
				continue
			}
			ports = append(ports, p)
		}
	}
	return sortedPorts(ports)
}

// sortedPorts returns the ports sorted, once each.
func sortedPorts(ports []int) []int {
	slices.Sort(ports)
	return slices.Compact(ports)
}

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
	return sortedPorts(ports)
}

// prepareLauncher checks that the launcher can run the app on its public ports, then brings it up
// publishing the ports of every app on the board, those of the app included: an app changed after
// the others were collected still recreates the launcher on its new ports.
func prepareLauncher(
	ctx context.Context,
	docker command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	appToStart app.ArduinoApp,
	ports []int,
	cfg config.Configuration,
	platform platform.Platform,
	cb func(StreamMessage),
) error {
	if roots := launcherAppRoots(cfg); !isInsideLauncherRoots(appToStart.FullPath, cfg) {
		return fmt.Errorf("the app launcher runs only the apps inside %s", strings.Join(roots.AsStrings(), " or "))
	}
	for _, p := range ports {
		if p < 1 || p > maxPort {
			return fmt.Errorf("the app launcher cannot publish port %d: the app ports go from 1 to %d", p, maxPort)
		}
	}
	published := sortedPorts(append(launcherPorts(cfg, bricksIndex), ports...))
	elapsed, err := ensureLauncher(ctx, docker, published, cfg, platform, false, cb)
	if err != nil {
		return fmt.Errorf("the app launcher is unavailable: %w", err)
	}
	cb(StreamMessage{data: fmt.Sprintf("App launcher ready in %s", elapsed.Round(time.Millisecond))})
	return nil
}

// launcherAppRoots are the folders mounted in the launcher at their host path: the apps run in place,
// the installed releases included.
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

// generateLauncherComposeFile writes the launcher compose project, rendered on this board and
// publishing the ports, and tells whether it changed. Docker fixes the published ports at
// container creation: a new port changes the file, which recreates the launcher.
func generateLauncherComposeFile(
	ports []int,
	pythonImage string,
	cfg config.Configuration,
	boardEnvs types.Mapping,
	platform platform.Platform,
) (bool, error) {
	ports = sortedPorts(slices.Clone(ports))
	published := make([]string, 0, len(ports))
	for _, p := range ports {
		published = append(published, fmt.Sprintf("%d:%d", p, p))
	}

	envs := boardEnvs.Clone()
	envs["APP_LAUNCHER_SOCKET"] = launcherSocketDir + "/" + launcher.SocketName
	envs["APP_LAUNCHER_PREPARE_TIMEOUT_S"] = strconv.Itoa(int(launcherPrepareTimeout.Seconds()))
	if cfg.LauncherPreload != "" {
		envs["APP_LAUNCHER_PRELOAD"] = cfg.LauncherPreload // Part of the compose file: changing it recreates the launcher
	}

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

	changed, err := writeFileIfChanged(launcherComposeFile(cfg), data)
	if err != nil {
		return false, fmt.Errorf("failed to write the launcher compose file: %w", err)
	}
	return changed, nil
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
	delete(envs, "APP_HOME")
	delete(envs, "VIDEO_DEVICE")
	envs["BOARD_NAME"] = plat.BoardName
	return envs
}

// ensureLauncher brings the launcher up to date, publishing the ports, and running, and waits until
// it answers. forceUp runs compose up even when nothing changed here, so that a new image under the
// same tag is picked up: compose recreates the container only when its configuration or image differ.
func ensureLauncher(
	ctx context.Context,
	docker command.Cli,
	ports []int,
	cfg config.Configuration,
	platform platform.Platform,
	forceUp bool,
	cb func(StreamMessage),
) (time.Duration, error) {
	started := time.Now()
	if err := cfg.RuntimeDir().MkdirAll(); err != nil {
		return 0, fmt.Errorf("failed to create the runtime folder: %w", err)
	}
	changed, err := generateLauncherComposeFile(ports, cfg.PythonImage, cfg, boardEnvironment(ctx, platform, cfg), platform)
	if err != nil {
		return 0, err
	}
	rendered := time.Since(started)
	// An unchanged compose file and a launcher answering on its socket: up to date and running,
	// the Docker round trips below have nothing to add
	if !forceUp && !changed {
		if err := newLauncherClient(cfg).WithTimeout(pingTimeout).Ping(ctx); err == nil {
			elapsed := time.Since(started)
			slog.Debug("app launcher answers, up to date", slog.Duration("elapsed", elapsed), slog.Duration("render", rendered))
			return elapsed, nil
		}
	}

	current, err := getContainerWithLabel(ctx, docker.Client(), DockerLauncherLabel)
	if err != nil {
		return 0, err
	}
	listed := time.Since(started) - rendered
	running := current != nil && current.State == container.StateRunning
	recreated := false
	if reason, up := composeUpReason(forceUp, changed, running); up {
		slog.Debug("running compose up for the app launcher", slog.String("reason", reason))
		cb(StreamMessage{data: "Starting the app launcher"})
		if err := composeUpFile(ctx, docker, launcherComposeFile(cfg), cb); err != nil {
			return 0, fmt.Errorf("failed to start the app launcher: %w", err)
		}
		previous := current
		if current, err = getContainerWithLabel(ctx, docker.Client(), DockerLauncherLabel); err != nil {
			return 0, err
		}
		if current == nil {
			return 0, errors.New("the app launcher container was not created")
		}
		recreated = previous == nil || previous.ID != current.ID || !running
		slog.Debug("app launcher compose up done", slog.Bool("recreated", recreated))
	} else {
		slog.Debug("app launcher up to date and running, compose up skipped")
	}
	// A new or restarted launcher runs no app
	if recreated {
		if err := clearLauncherWantedApp(cfg, nil); err != nil {
			return 0, err
		}
	}

	if err := waitLauncherReady(ctx, docker.Client(), newLauncherClient(cfg), current.ID); err != nil {
		return 0, err
	}
	elapsed := time.Since(started)
	slog.Info("app launcher ready", slog.Duration("elapsed", elapsed), slog.Bool("recreated", recreated),
		slog.Duration("render", rendered), slog.Duration("list", listed))
	return elapsed, nil
}

// composeUpReason tells whether compose up has to run for the launcher, and why.
func composeUpReason(forceUp, changed, running bool) (string, bool) {
	switch {
	case changed:
		return "compose file changed", true
	case !running:
		return "container not running", true
	case forceUp:
		return "forced, to follow the image", true
	default:
		return "", false
	}
}

// pingTimeout bounds one ping of the launcher, a local socket round trip.
const pingTimeout = 500 * time.Millisecond

// waitLauncherReady polls ping until the preload is over. It fails early when the container stops
// or restarts, as an image without the serve mode does.
func waitLauncherReady(ctx context.Context, docker dockerClient.APIClient, client *launcher.Client, id string) error {
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

// EnsureLauncher brings the launcher up on the ports of the apps on the board, recreating it on a
// new image or port. When stopApps, as the daemon does at boot, it stops what the launcher runs.
func EnsureLauncher(
	ctx context.Context,
	docker command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	cfg config.Configuration,
	platform platform.Platform,
	stopApps bool,
) (time.Duration, error) {
	started := time.Now()
	err := withAppsLock(ctx, cfg, func() error {
		if _, err := ensureLauncher(ctx, docker, launcherPorts(cfg, bricksIndex), cfg, platform, true, func(StreamMessage) {}); err != nil {
			return err
		}
		if !stopApps {
			return nil
		}
		return stopLauncherApp(ctx, cfg, nil)
	})
	return time.Since(started), err
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
