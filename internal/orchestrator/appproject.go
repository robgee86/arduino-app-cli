// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// appProject is the compose project of an app as rendered on this board, its main service taken
// out: the launcher runs the app, the sidecars run beside it in the project of the app, as on main,
// on the network of the launcher instead of one of their own, so that the launcher reaches them by
// name. The project files stay as they are generated, and frozen in a release.
type appProject struct {
	name string
	prj  *types.Project
	// fingerprint tells the sidecars apart from the ones their containers were created from.
	fingerprint string
	// appEnvs is what the main service would have run the app with: the environment the launcher gives the app.
	appEnvs map[string]string
}

func newAppProject(prj *types.Project) (appProject, error) {
	appEnvs := splitMainService(prj)
	prj.Networks = types.Networks{"default": types.NetworkConfig{Name: launcherNetwork, External: true}}
	data, err := json.Marshal(prj.Services)
	if err != nil {
		return appProject{}, err
	}
	sum := sha256.Sum256(data)
	return appProject{name: prj.Name, prj: prj, fingerprint: hex.EncodeToString(sum[:]), appEnvs: appEnvs}, nil
}

// sidecars are the services of the project, the main one gone.
func (p appProject) sidecars() []string {
	return p.prj.ServiceNames()
}

// projectRecordFile holds the fingerprint of the project the sidecars of the app were created from.
func projectRecordFile(cfg config.Configuration, project string) *paths.Path {
	return cfg.DataDir().Join("apps", project, "project.json")
}

type projectRecord struct {
	Fingerprint string `json:"fingerprint"`
}

func (p appProject) recordedFingerprint(cfg config.Configuration) string {
	data, err := projectRecordFile(cfg, p.name).ReadFile()
	if err != nil {
		return ""
	}
	var record projectRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return ""
	}
	return record.Fingerprint
}

func (p appProject) recordFingerprint(cfg config.Configuration) error {
	data, err := json.Marshal(projectRecord{Fingerprint: p.fingerprint})
	if err != nil {
		return err
	}
	file := projectRecordFile(cfg, p.name)
	if err := file.Parent().MkdirAll(); err != nil {
		return err
	}
	return fatomic.WriteFile(file.String(), data, 0o644)
}

// ensureSidecars brings the sidecars of the app up as the project states them, with compose when the
// project changed or a container is missing, by starting the stopped ones otherwise, and waits until
// they are ready the way compose waited for the dependencies of the main service: healthy when they
// have a healthcheck, running otherwise. The app is forked only then, as it was started only then.
func ensureSidecars(ctx context.Context, docker command.Cli, cfg config.Configuration, p appProject, cb func(StreamMessage)) error {
	if len(p.sidecars()) == 0 {
		return nil
	}
	containers, err := getAppContainers(ctx, docker.Client(), p.name)
	if err != nil {
		return err
	}
	if p.recordedFingerprint(cfg) != p.fingerprint || !containers.cover(p.sidecars()) {
		slog.Debug("bringing the app sidecars up", slog.String("project", p.name), slog.Any("services", p.sidecars()))
		cb(StreamMessage{data: "Starting the app sidecars"})
		if err := dockerhelper.ComposeUp(ctx, docker, p.prj, func(line string) { cb(StreamMessage{data: line}) }); err != nil {
			return err
		}
		if err := p.recordFingerprint(cfg); err != nil {
			return err
		}
	} else if err := startAppContainers(ctx, docker.Client(), containers.notRunning(), cb); err != nil {
		return err
	}
	return waitSidecarsReady(ctx, docker.Client(), p)
}

// waitSidecarsReady waits for every sidecar of the project to be ready, see ensureSidecars.
func waitSidecarsReady(ctx context.Context, docker dockerClient.APIClient, p appProject) error {
	if len(p.sidecars()) == 0 {
		return nil
	}
	containers, err := getAppContainers(ctx, docker, p.name)
	if err != nil {
		return err
	}
	var (
		errs []error
		wg   sync.WaitGroup
		mu   sync.Mutex
	)
	for name, svc := range p.prj.Services {
		ctr, ok := containers[name]
		if !ok {
			return fmt.Errorf("the sidecar %s has no container", name)
		}
		wg.Go(func() {
			var err error
			if svc.HealthCheck != nil && !svc.HealthCheck.Disable {
				err = waitContainerHealthy(ctx, docker, ctr.ID)
			} else {
				err = waitContainerRunning(ctx, docker, ctr.ID)
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("the sidecar %s is not ready: %w", containerName(ctr), err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopProjectSidecars stops the sidecars of the app project, which stay for the next start.
func stopProjectSidecars(ctx context.Context, docker dockerClient.APIClient, project string, cb func(StreamMessage)) error {
	containers, err := getAppContainers(ctx, docker, project)
	if err != nil {
		return err
	}
	return stopAppContainers(ctx, docker, containers, DefaultDockerStopTimeoutSeconds, cb)
}

// destroyAppProject removes the project of the app, its sidecars and their volumes, with its record.
func destroyAppProject(ctx context.Context, docker command.Cli, cfg config.Configuration, project string) error {
	if err := dockerhelper.ComposeDown(ctx, docker, project, func(string) {}); err != nil {
		return fmt.Errorf("failed to remove the containers of %s: %w", project, err)
	}
	return projectRecordFile(cfg, project).Parent().RemoveAll()
}

// removeSharedSidecars removes the sidecars project of the previous layout and its network.
func removeSharedSidecars(ctx context.Context, docker command.Cli) {
	const sharedProject, sharedNetwork = "arduino-sidecars", "arduino-apps"
	if containers, err := getAppContainers(ctx, docker.Client(), sharedProject); err == nil && len(containers) > 0 {
		slog.Info("removing the sidecars project shared between the apps")
		if err := dockerhelper.ComposeDown(ctx, docker, sharedProject, func(string) {}); err != nil {
			slog.Warn("failed to remove the shared sidecars project", slog.String("error", err.Error()))
		}
	}
	if _, err := docker.Client().NetworkInspect(ctx, sharedNetwork, dockerClient.NetworkInspectOptions{}); err == nil {
		if _, err := docker.Client().NetworkRemove(ctx, sharedNetwork, dockerClient.NetworkRemoveOptions{}); err != nil {
			slog.Debug("the shared apps network stays", slog.String("error", err.Error()))
		}
	}
}

// waitContainerHealthy waits for the healthcheck of a running container to pass, as compose does for
// a service_healthy dependency. It fails when the container exits or its healthcheck fails.
func waitContainerHealthy(ctx context.Context, docker dockerClient.APIClient, id string) error {
	deadline := time.Now().Add(sidecarReadyTimeout)
	for {
		inspected, err := docker.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		info := inspected.Container
		switch {
		case info.State == nil:
			return errors.New("the container has no state")
		case !info.State.Running && !info.State.Restarting && info.State.Status != container.StateCreated:
			return fmt.Errorf("the container %s with code %d", info.State.Status, info.State.ExitCode)
		case info.State.Health == nil:
			return errors.New("the container has no healthcheck")
		case info.State.Health.Status == container.Healthy:
			return nil
		case info.State.Health.Status == container.Unhealthy:
			return errors.New("the container is unhealthy")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still %s after %s", info.State.Health.Status, sidecarReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}
