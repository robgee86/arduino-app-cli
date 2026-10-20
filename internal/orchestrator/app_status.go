// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/docker/cli/cli/command"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/appid"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// The supervisor of an app container has no events: its child is polled while someone listens.
const launcherPollInterval = time.Second

// AppStatusEvents yields an app each time its status changes. Container events refresh the
// containers, a ticker refreshes the child of the wanted app.
func AppStatusEvents(ctx context.Context, cfg config.Configuration, docker command.Cli, idProvider *appid.Provider) iter.Seq2[AppInfo, error] {
	chanMsg, chanError := dockerhelper.ContainerEvents(ctx, docker.Client(), DockerAppLabel+"=true",
		"create", "start", "stop", "die", "restart", "destroy", "delete")

	return func(yield func(AppInfo, error) bool) {
		containers, err := listAppContainers(ctx, docker.Client())
		if err != nil {
			_ = yield(AppInfo{}, err)
			return
		}
		last := statusByPath(mergeAppStatus(containers, readAppsView(ctx, docker.Client(), cfg)))

		ticker := time.NewTicker(launcherPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				slog.Debug("Stopping to listen to docker events")
				return
			case err := <-chanError:
				if err != nil {
					slog.Error("Error listening to docker events", slog.String("error", err.Error()))
					_ = yield(AppInfo{}, fmt.Errorf("error listening to docker events: %w", err))
					return
				}
			case <-chanMsg:
				if containers, err = listAppContainers(ctx, docker.Client()); err != nil {
					slog.Error("Unable to get apps status", slog.String("error", err.Error()))
					if !yield(AppInfo{}, err) {
						return
					}
					continue
				}
			case <-ticker.C:
			}

			current := statusByPath(mergeAppStatus(containers, readAppsView(ctx, docker.Client(), cfg)))
			for _, change := range statusChanges(last, current) {
				info, err := appInfoFromStatus(cfg, idProvider, change)
				if err != nil {
					slog.Warn("skipping the status of an app", slog.String("path", change.AppPath.String()), slog.String("error", err.Error()))
					continue
				}
				if !yield(info, nil) {
					return
				}
			}
			last = current
		}
	}
}

func statusByPath(statuses []AppStatusInfo) map[string]Status {
	result := make(map[string]Status, len(statuses))
	for _, s := range statuses {
		result[s.AppPath.String()] = s.Status
	}
	return result
}

// statusChanges lists the apps whose status differs, sorted by path. An app that is gone is idle.
func statusChanges(last, current map[string]Status) []AppStatusInfo {
	var changes []AppStatusInfo
	for appPath, status := range current {
		if last[appPath] != status {
			changes = append(changes, AppStatusInfo{AppPath: paths.New(appPath), Status: status})
		}
	}
	for appPath := range last {
		if _, ok := current[appPath]; !ok {
			p := paths.New(appPath)
			if status := idleAppStatus(p); status != last[appPath] {
				changes = append(changes, AppStatusInfo{AppPath: p, Status: status})
			}
		}
	}
	slices.SortFunc(changes, func(a, b AppStatusInfo) int { return strings.Compare(a.AppPath.String(), b.AppPath.String()) })
	return changes
}

func appInfoFromStatus(cfg config.Configuration, idProvider *appid.Provider, status AppStatusInfo) (AppInfo, error) {
	app, err := app.Load(status.AppPath)
	if err != nil {
		return AppInfo{}, err
	}

	defaultApp, err := GetDefaultApp(cfg)
	if err != nil {
		slog.Warn("unable to get default app", slog.String("error", err.Error()))
	}

	id, err := idProvider.IDFromPath(status.AppPath)
	if err != nil {
		return AppInfo{}, err
	}

	isDefault := defaultApp != nil && defaultApp.FullPath.EqualsTo(app.FullPath)

	return AppInfo{
		ID:          id,
		Name:        app.Descriptor.Name,
		Description: app.Descriptor.Description,
		Icon:        app.Descriptor.Icon,
		Status:      status.Status,
		Example:     id.IsExample(),
		Default:     isDefault,
	}, nil
}
