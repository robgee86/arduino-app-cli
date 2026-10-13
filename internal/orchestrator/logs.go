// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"go.bug.st/f"

	"github.com/arduino/go-paths-helper"

	"github.com/arduino/arduino-app-cli/internal/helpers"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/servicesindex"
)

type AppLogsRequest struct {
	ShowAppLogs      bool
	ShowServicesLogs bool
	Follow           bool
	Tail             *uint64
}

type LogSource string

const (
	LogSourceMain  LogSource = "main"
	LogSourceBrick LogSource = "brick"
)

type LogMessage struct {
	Source        LogSource
	BrickID       string // empty when Source != LogSourceBrick
	ContainerName string
	Content       string
}

func AppLogs(
	ctx context.Context,
	app app.ArduinoApp,
	req AppLogsRequest,
	dockerCli command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	servicesIndex *servicesindex.ServicesIndex,
	cfg config.Configuration,
) (iter.Seq[LogMessage], error) {
	if app.MainPythonFile == nil {
		return helpers.EmptyIter[LogMessage](), nil
	}

	services, err := getAppServicesFromContainers(ctx, dockerCli.Client(), app)
	if err != nil {
		return nil, err
	}

	// A main container means the app last ran in classic mode, with its logs in Docker
	if slices.Contains(services, mainServiceName) {
		source, err := composeLogs(app, req, services, dockerCli, bricksIndex, servicesIndex, cfg)
		if err != nil {
			return nil, err
		}
		return func(yield func(LogMessage) bool) { source(ctx, yield) }, nil
	}

	// The launcher writes the app output to a file, Docker holds the sidecar logs
	var sources []logSource
	if req.ShowAppLogs {
		sources = append(sources, func(ctx context.Context, yield func(LogMessage) bool) {
			readAppLogFile(ctx, appLogFile(app), req.Tail, req.Follow, yield)
		})
	}
	if req.ShowServicesLogs && len(services) > 0 {
		source, err := composeLogs(app, AppLogsRequest{ShowServicesLogs: true, Follow: req.Follow, Tail: req.Tail}, services, dockerCli, bricksIndex, servicesIndex, cfg)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return mergeLogSources(ctx, sources...), nil
}

// logSource yields log lines until yield returns false or ctx is done.
type logSource func(ctx context.Context, yield func(LogMessage) bool)

// mergeLogSources yields the lines of every source as they come.
func mergeLogSources(ctx context.Context, sources ...logSource) iter.Seq[LogMessage] {
	return func(yield func(LogMessage) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		messages := make(chan LogMessage)
		var wg sync.WaitGroup
		for _, source := range sources {
			wg.Go(func() {
				source(ctx, func(msg LogMessage) bool {
					select {
					case messages <- msg:
						return true
					case <-ctx.Done():
						return false
					}
				})
			})
		}
		go func() {
			wg.Wait()
			close(messages)
		}()
		for msg := range messages {
			if !yield(msg) {
				cancel()
				// Drain so that the sources see the cancellation and the closer goroutine ends
				for range messages {
				}
				return
			}
		}
	}
}

// composeLogs yields the Docker logs of the app containers, attributing each service to its brick.
func composeLogs(
	app app.ArduinoApp,
	req AppLogsRequest,
	services []string,
	dockerCli command.Cli,
	bricksIndex *bricksindex.BricksIndex,
	servicesIndex *servicesindex.ServicesIndex,
	cfg config.Configuration,
) (logSource, error) {
	projectName := composeProjectName(app.FullPath, cfg.AppsDir())

	serviceToBrickMapping, err := serviceBricks(app, bricksIndex, servicesIndex)
	if err != nil {
		return nil, err
	}

	if req.ShowAppLogs && !req.ShowServicesLogs {
		services = []string{mainServiceName}
	} else if req.ShowServicesLogs && !req.ShowAppLogs {
		services = f.Filter(services, f.NotEquals(mainServiceName))
	}
	// An empty service list makes compose show every container of the project
	if len(services) == 0 {
		return func(context.Context, func(LogMessage) bool) {}, nil
	}

	backend, err := compose.NewComposeService(dockerCli)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, yield func(LogMessage) bool) {
		opts := api.LogOptions{
			Follow:     req.Follow,
			Services:   services,
			Timestamps: false,
		}
		if req.Tail != nil {
			opts.Tail = fmt.Sprintf("%d", *req.Tail)
		}
		err := backend.Logs(
			ctx,
			projectName,
			NewDockerLogConsumer(ctx, yield, serviceToBrickMapping),
			opts,
		)
		if err != nil {
			slog.Error("docker logs error", slog.String("error", err.Error()))
			return
		}
	}, nil
}

// serviceBricks maps each compose service of the app to the brick that declares or requires it.
func serviceBricks(app app.ArduinoApp, bricksIndex *bricksindex.BricksIndex, servicesIndex *servicesindex.ServicesIndex) (map[string]string, error) {
	bricksIndex = bricksIndex.WithAppBricks(app.LocalBricks)

	serviceToBrickMapping := make(map[string]string, len(app.Descriptor.Bricks))
	addServices := func(composeFile *paths.Path, brickID string) error {
		services, err := extractServicesFromComposeFile(composeFile)
		if err != nil {
			return fmt.Errorf("brick %s: invalid compose file %s: %w", brickID, appRelative(composeFile, app.FullPath), err)
		}
		for _, s := range services {
			if _, claimed := serviceToBrickMapping[s.name]; !claimed {
				serviceToBrickMapping[s.name] = brickID
			}
		}
		return nil
	}
	for _, appBrick := range app.Descriptor.Bricks {
		brick, ok := bricksIndex.FindBrickByID(appBrick.ID)
		if !ok {
			slog.Warn("brick not valid", slog.String("brick_id", appBrick.ID))
			continue
		}

		if composeFile, found := brick.GetComposeFile(); found && composeFile.Exist() {
			if err := addServices(composeFile, brick.ID); err != nil {
				return nil, err
			}
		} else {
			slog.Debug("brick has no compose file", slog.String("brick_id", brick.ID))
		}

		// Containers of an Arduino Service belong to the brick that requires it.
		requiredServices, err := brick.GetMatchingService(bricksindex.BrickInstance{
			Model: cmp.Or(appBrick.Model, brick.ModelName),
		})
		if err != nil {
			slog.Warn("failed to get required services for brick", slog.String("brick_id", brick.ID), slog.Any("error", err))
			continue
		}
		for _, serviceID := range requiredServices {
			service, found := servicesIndex.FindServiceByID(serviceID)
			if !found {
				continue
			}
			composeFile, ok := service.GetComposeFile()
			if !ok {
				continue
			}
			slog.Debug("attributing service to brick", slog.String("service_id", serviceID), slog.String("brick_id", brick.ID))
			if err := addServices(composeFile, brick.ID); err != nil {
				slog.Warn("failed to load service compose", slog.String("service_id", serviceID), slog.Any("error", err))
			}
		}
	}
	return serviceToBrickMapping, nil
}

var _ api.LogConsumer = (*DockerLogConsumer)(nil)

type DockerLogConsumer struct {
	ctx          context.Context
	cb           func(LogMessage) bool
	mapping      map[string]string
	shuttingDown atomic.Bool
	mu           sync.Mutex
}

func NewDockerLogConsumer(
	ctx context.Context,
	cb func(LogMessage) bool,
	mapping map[string]string,
) *DockerLogConsumer {
	return &DockerLogConsumer{
		ctx:     ctx,
		cb:      cb,
		mapping: mapping,
	}
}

// Err implements api.LogConsumer.
func (d *DockerLogConsumer) Err(containerName string, message string) {
	d.write(containerName, message)
}

// Log implements api.LogConsumer.
func (d *DockerLogConsumer) Log(containerName string, message string) {
	d.write(containerName, message)
}

// Status implements api.LogConsumer.
func (d *DockerLogConsumer) Status(container string, msg string) {
	d.write(container, msg)
}

func (d *DockerLogConsumer) write(container, message string) {
	if d.ctx.Err() != nil || d.shuttingDown.Load() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shuttingDown.Load() {
		return
	}

	serviceName := strings.TrimSpace(container)
	idx := strings.LastIndex(serviceName, "-")
	if idx != -1 {
		// remove the suffix -1 or -2 or -4
		serviceName = serviceName[:idx]
	}

	msg := LogMessage{Source: LogSourceMain, ContainerName: serviceName}
	if brickID, ok := d.mapping[serviceName]; ok {
		msg.Source = LogSourceBrick
		msg.BrickID = brickID
	}
	for line := range strings.SplitSeq(message, "\n") {
		msg.Content = line
		if !d.cb(msg) {
			d.shuttingDown.CompareAndSwap(false, true)
			return
		}
	}
}
