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

// sidecarsStatus is the overall status of the zone containers of the services, all of them when
// services is nil, empty without containers or when it cannot tell:
//
//	running: all running
//	stopped: all stopped
//	failed: at least one failed
//	stopping: at least one stopping
//	starting: at least one starting
func sidecarsStatus(containers []container.Summary, services []string) Status {
	zone, _ := splitAppContainers(containers)
	if services != nil {
		zone = zone.only(services)
	}
	s := make([]Status, 0, len(zone))
	for _, c := range zone {
		s = append(s, StatusFromDockerState(c.State, c.Status))
	}
	switch {
	case len(s) == 0:
		return ""
	case !slices.ContainsFunc(s, func(v Status) bool { return v != StatusRunning }):
		return StatusRunning
	case !slices.ContainsFunc(s, func(v Status) bool { return v != StatusStopped }):
		return StatusStopped
	case slices.Contains(s, StatusFailed):
		return StatusFailed
	case slices.Contains(s, StatusStopping):
		return StatusStopping
	case slices.Contains(s, StatusStarting):
		return StatusStarting
	default:
		return ""
	}
}

// launcherView is what the launcher runs, the app it should be running, and the zone services
// that are its sidecars: nil without record, then every zone container counts.
type launcherView struct {
	children []launcher.Child
	wanted   *paths.Path
	sidecars []string
}

// readLauncherView asks the launcher for its children. An unreachable launcher runs nothing.
func readLauncherView(ctx context.Context, cfg config.Configuration) launcherView {
	view := launcherView{wanted: readLauncherWantedApp(cfg)}
	if record := readSidecarsRecord(cfg); record != nil {
		view.sidecars = slices.Collect(maps.Keys(record))
	}
	status, err := newLauncherClient(cfg).Status(ctx)
	if err != nil {
		slog.Debug("app launcher status unavailable", slog.String("error", err.Error()))
		return view
	}
	view.children = status.Children
	return view
}

// mergeAppStatus combines the sidecars zone with the launcher children: the zone runs the sidecars
// of the wanted app. An app neither run nor wanted is idle: its sidecar exit codes come from the
// stop, and a runner may exit 0 on SIGTERM.
func mergeAppStatus(containers []container.Summary, view launcherView) []AppStatusInfo {
	// The orphans being removed alongside a start are no sidecars of the app
	zone := sidecarsStatus(containers, view.sidecars)
	children := make(map[string]launcher.Child, len(view.children))
	for _, c := range view.children {
		children[filepath.Clean(c.App)] = c
	}

	appPaths := slices.Collect(maps.Keys(children))
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
			var sidecars Status
			if wanted {
				sidecars = zone
			}
			status = launcherAppStatus(sidecars, child, running)
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

// listAppContainers lists the app containers, running or not: the sidecars zone and the per-app projects of the older versions.
func listAppContainers(ctx context.Context, docker dockerClient.APIClient) ([]container.Summary, error) {
	containers, err := dockerhelper.Containers(ctx, docker, DockerAppLabel+"=true")
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
	containers, err := listAppContainers(ctx, docker)
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
	statuses, err := getAppsStatus(ctx, docker, cfg)
	if err != nil {
		return AppStatusInfo{}, err
	}
	for _, s := range statuses {
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
