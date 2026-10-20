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

// appsView is the app that should be running with the child its container runs, and the zone
// services that are its sidecars: nil without record, then every zone container counts.
type appsView struct {
	wanted   *paths.Path
	child    *launcher.Child
	sidecars []string
}

// readAppsView asks the container of the wanted app for its child. An unreachable container runs nothing.
func readAppsView(ctx context.Context, cfg config.Configuration) appsView {
	var view appsView
	if record := readSidecarsRecord(cfg); record != nil {
		view.sidecars = slices.Collect(maps.Keys(record))
	}
	wanted := readWantedApp(cfg)
	if wanted == nil {
		return view
	}
	view.wanted = wanted.App
	status, err := appContainerOf(cfg, wanted.App).client(cfg).Status(ctx)
	if err != nil {
		slog.Debug("app container status unavailable", slog.String("error", err.Error()))
		return view
	}
	view.child = status.Child
	return view
}

// mergeAppStatus combines the sidecars zone with the child of the wanted app: the zone runs its
// sidecars. Every other app with a container is idle: its sidecar exit codes come from the stop,
// and a runner may exit 0 on SIGTERM.
func mergeAppStatus(containers []container.Summary, view appsView) []AppStatusInfo {
	// The orphans being removed alongside a start are no sidecars of the app
	zone := sidecarsStatus(containers, view.sidecars)
	statuses := make(map[string]Status)
	for _, c := range containers {
		if appPath := c.Labels[DockerAppPathLabel]; c.Labels[DockerAppMainLabel] == "true" && appPath != "" {
			statuses[filepath.Clean(appPath)] = idleAppStatus(paths.New(appPath))
		}
	}
	if view.wanted != nil {
		statuses[view.wanted.String()] = wantedAppStatus(zone, view.child)
	}

	result := make([]AppStatusInfo, 0, len(statuses))
	for _, appPath := range slices.Sorted(maps.Keys(statuses)) {
		result = append(result, AppStatusInfo{AppPath: paths.New(appPath), Status: statuses[appPath]})
	}
	return result
}

// wantedAppStatus is the status of the app that should be running; sidecars is empty without sidecars.
func wantedAppStatus(sidecars Status, child *launcher.Child) Status {
	switch {
	// A failed sidecar, or a wanted app whose child is gone
	case sidecars == StatusFailed, child == nil:
		return StatusFailed
	case child.Stopping:
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

// listAppContainers lists the app containers, running or not: the containers of the apps, the
// sidecars zone and the sidecars of the per-app projects of the older versions.
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
	return mergeAppStatus(containers, readAppsView(ctx, cfg)), nil
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
