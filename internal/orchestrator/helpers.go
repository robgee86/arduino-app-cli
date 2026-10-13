// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	rpc "github.com/arduino/arduino-cli/rpc/cc/arduino/cli/commands/v1"
	"github.com/arduino/go-paths-helper"
	"github.com/gosimple/slug"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
	"go.bug.st/f"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

type AppStatusInfo struct {
	AppPath *paths.Path
	Status  Status
}

// parseAppStatus takes all the containers that matches the DockerAppLabel,
// and construct a map of the state of an app and all its dependencies state.
// For app that have at least 1 dependency, we calculate the overall state
// as follow:
//
//	running: all running
//	stopped: all stopped
//	failed: at least one failed
//	stopping: at least one stopping
//	starting: at least one starting
func parseAppStatus(containers []container.Summary) []AppStatusInfo {
	apps := make([]AppStatusInfo, 0, len(containers))
	appsStatusMap := make(map[string][]Status)
	for _, c := range containers {
		appPath, ok := c.Labels[DockerAppPathLabel]
		if !ok {
			continue
		}
		appsStatusMap[appPath] = append(appsStatusMap[appPath], StatusFromDockerState(c.State, c.Status))

	}

	appendResult := func(appPath *paths.Path, status Status) {
		apps = append(apps, AppStatusInfo{
			AppPath: appPath,
			Status:  status,
		})
	}

	for appPath, s := range appsStatusMap {
		f.Assert(len(s) != 0, "status slice is zero")

		appPath := paths.New(appPath)

		//	running: all running
		if !slices.ContainsFunc(s, func(v Status) bool { return v != StatusRunning }) {
			appendResult(appPath, StatusRunning)
			continue
		}
		//	stopped: all stopped
		if !slices.ContainsFunc(s, func(v Status) bool { return v != StatusStopped }) {
			appendResult(appPath, StatusStopped)
			continue
		}

		// ...else we have multiple different status we calculate the status
		// among the possible left: {failed, stopping, starting}
		if slices.ContainsFunc(s, func(v Status) bool { return v == StatusFailed }) {
			appendResult(appPath, StatusFailed)
			continue
		}
		if slices.ContainsFunc(s, func(v Status) bool { return v == StatusStopping }) {
			appendResult(appPath, StatusStopping)
			continue
		}
		if slices.ContainsFunc(s, func(v Status) bool { return v == StatusStarting }) {
			appendResult(appPath, StatusStarting)
			continue
		}
	}

	return apps
}

// launcherView is what the launcher runs, and the app it should be running.
type launcherView struct {
	children []launcher.Child
	wanted   *paths.Path
}

// readLauncherView asks the launcher for its children. An unreachable launcher runs nothing.
func readLauncherView(ctx context.Context, cfg config.Configuration) launcherView {
	view := launcherView{wanted: readLauncherWantedApp(cfg)}
	status, err := newLauncherClient(cfg).Status(ctx)
	if err != nil {
		slog.Debug("app launcher status unavailable", slog.String("error", err.Error()))
		return view
	}
	view.children = status.Children
	return view
}

// mergeAppStatus combines the app sidecars with the launcher children. An app neither run nor
// wanted is idle: its sidecar exit codes come from the stop, and a runner may exit 0 on SIGTERM.
func mergeAppStatus(containers []container.Summary, view launcherView) []AppStatusInfo {
	sidecars := make(map[string]Status)
	for _, s := range parseAppStatus(containers) {
		sidecars[s.AppPath.String()] = s.Status
	}
	children := make(map[string]launcher.Child, len(view.children))
	for _, c := range view.children {
		children[filepath.Clean(c.App)] = c
	}

	appPaths := slices.Concat(slices.Collect(maps.Keys(sidecars)), slices.Collect(maps.Keys(children)))
	if view.wanted != nil {
		appPaths = append(appPaths, view.wanted.String())
	}
	slices.Sort(appPaths)

	result := make([]AppStatusInfo, 0, len(appPaths))
	for _, appPath := range slices.Compact(appPaths) {
		p := paths.New(appPath)
		child, running := children[appPath]
		wanted := view.wanted != nil && view.wanted.EqualsTo(p)
		status := idleAppStatus(p)
		if running || wanted {
			status = launcherAppStatus(sidecars[appPath], child, running)
		}
		result = append(result, AppStatusInfo{AppPath: p, Status: status})
	}
	return result
}

// launcherAppStatus is the status of an app the launcher runs or should run; sidecars is empty without sidecars.
func launcherAppStatus(sidecars Status, child launcher.Child, hasChild bool) Status {
	switch {
	// A failed sidecar, or a wanted app whose child is gone
	case sidecars == StatusFailed, !hasChild:
		return StatusFailed
	case child.State == launcher.StateStopping:
		return StatusStopping
	case sidecars == StatusStarting:
		return StatusStarting
	default:
		return StatusRunning
	}
}

// idleAppStatus is the status of an app that runs nothing: stopped once the CLI provisioned or
// ran it, which creates its .cache folder, uninitialized before.
func idleAppStatus(appPath *paths.Path) Status {
	if isDir, _ := appPath.Join(".cache").IsDirCheck(); isDir {
		return StatusStopped
	}
	return StatusUninitialized
}

func listAppContainers(ctx context.Context, docker dockerClient.APIClient, filter string) ([]container.Summary, error) {
	containers, err := dockerhelper.Containers(ctx, docker, filter)
	if err != nil {
		return nil, err
	}
	return containers, nil
}

func getAppsStatus(
	ctx context.Context,
	docker dockerClient.APIClient,
	cfg config.Configuration,
) ([]AppStatusInfo, error) {
	containers, err := listAppContainers(ctx, docker, DockerAppLabel+"=true")
	if err != nil {
		return nil, err
	}
	return mergeAppStatus(containers, readLauncherView(ctx, cfg)), nil
}

func getAppStatus(
	ctx context.Context,
	docker dockerClient.APIClient,
	app app.ArduinoApp,
	cfg config.Configuration,
) (AppStatusInfo, error) {
	containers, err := listAppContainers(ctx, docker, DockerAppPathLabel+"="+app.FullPath.String())
	if err != nil {
		return AppStatusInfo{}, err
	}
	for _, s := range mergeAppStatus(containers, readLauncherView(ctx, cfg)) {
		if s.AppPath.EqualsTo(app.FullPath) {
			return s, nil
		}
	}
	return AppStatusInfo{AppPath: app.FullPath, Status: idleAppStatus(app.FullPath)}, nil
}

func getRunningApp(
	ctx context.Context,
	docker dockerClient.APIClient,
	cfg config.Configuration,
) (*app.ArduinoApp, error) {
	apps, err := getAppsStatus(ctx, docker, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to get running apps: %w", err)
	}
	idx := slices.IndexFunc(apps, func(a AppStatusInfo) bool {
		return a.Status == StatusRunning || a.Status == StatusStarting
	})
	if idx == -1 {
		return nil, nil
	}
	app, err := app.Load(apps[idx].AppPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load running app: %w", err)
	}
	return &app, nil
}

// getAppServicesFromContainers reads the services of an app from its containers. The
// compose file is not a source: an update removes the brick files that it includes.
func getAppServicesFromContainers(ctx context.Context, docker dockerClient.APIClient, app app.ArduinoApp) ([]string, error) {
	containers, err := dockerhelper.Containers(ctx, docker, DockerAppPathLabel+"="+app.FullPath.String())
	if err != nil {
		return nil, err
	}

	const dockerComposeServiceLabel = "com.docker.compose.service"
	services := make([]string, 0, len(containers))
	for _, info := range containers {
		if name := info.Labels[dockerComposeServiceLabel]; name != "" && !slices.Contains(services, name) {
			services = append(services, name)
		}
	}
	return services, nil
}

// composeProjectName is the docker project an app installed at appPath runs as. An app
// of the board is named after the apps dir, anything else after the path it is at.
func composeProjectName(appPath *paths.Path, appsDir *paths.Path) string {
	if relPath, err := appPath.RelFrom(appsDir); err == nil {
		return slug.Make(relPath.String())
	}
	return slug.Make(appPath.String())
}

func findAppPathByName(name string, cfg config.Configuration) (*paths.Path, bool) {
	appFolderName := slug.Make(name)
	basePath := cfg.AppsDir().Join(appFolderName)
	return basePath, basePath.Exist()
}

type ledTarget string

const (
	ledTriggerTarget    ledTarget = "trigger"
	ledBrightnessTarget ledTarget = "brightness"
)

const (
	noneTrigger    string = "none"
	defaultTrigger string = "default"
)

func setLeds(ledPath *paths.Path, target ledTarget, value string) error {
	ledPath = ledPath.Join(string(target))
	if !ledPath.Exist() {
		return fmt.Errorf("LED path %s does not exist", ledPath)
	}
	if err := ledPath.WriteFile([]byte(value)); err != nil {
		return fmt.Errorf("failed to set LED %s to %s: %w", ledPath, target, err)
	}
	return nil
}

func setLedsToUserControlledMode(platform platform.Platform) error {
	for _, led := range platform.Linux.BoardLeds {
		if err := setLeds(led, ledTriggerTarget, noneTrigger); err != nil {
			return err
		}
	}
	return nil
}

func restoreLedsState(platform platform.Platform) error {
	for _, led := range platform.Linux.BoardLeds {
		if err := setLeds(led, ledBrightnessTarget, string("0")); err != nil {
			return err
		}
		if err := setLeds(led, ledTriggerTarget, defaultTrigger); err != nil {
			return err
		}
	}
	return nil
}

func SetArduinoCliConfig(ctx context.Context, cli rpc.ArduinoCoreServiceServer) error {
	if _, err := cli.SettingsSetValue(ctx, &rpc.SettingsSetValueRequest{
		Key:          "network.connection_timeout",
		EncodedValue: "600s",
		ValueFormat:  "cli",
	}); err != nil {
		return err
	}

	// Set the data dir if specified via the ARDUINO_DIRECTORIES_DATA env var
	if dataDir, ok := os.LookupEnv("ARDUINO_DIRECTORIES_DATA"); ok {
		_, err := cli.SettingsSetValue(ctx, &rpc.SettingsSetValueRequest{
			Key:          "directories.data",
			EncodedValue: dataDir,
			ValueFormat:  "cli",
		})
		if err != nil {
			return fmt.Errorf("could not set data directory: %w", err)
		}
	}

	// Set the additional urls via ARDUINO_BOARD_MANAGER_ADDITIONAL_URLS env var
	if urls, ok := os.LookupEnv("ARDUINO_BOARD_MANAGER_ADDITIONAL_URLS"); ok {
		_, err := cli.SettingsSetValue(ctx, &rpc.SettingsSetValueRequest{
			Key:          "board_manager.additional_urls",
			EncodedValue: urls,
			ValueFormat:  "cli",
		})
		if err != nil {
			return fmt.Errorf("could not set additional urls: %w", err)
		}
	}

	return nil
}
