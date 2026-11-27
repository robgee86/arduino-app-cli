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
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
)

// healthPollInterval is how often the health of a dependency is read while it starts.
const healthPollInterval = 200 * time.Millisecond

// keptContainers are the containers a stop left behind for a project, in the order a
// start brings them up: the dependencies of the main service, then the main one.
type keptContainers struct {
	dependencies []keptContainer
	main         container.Summary
}

// keptContainer is a dependency of the main service, and whether the main service waits
// for its healthcheck to pass.
type keptContainer struct {
	container.Summary
	waitHealthy bool
}

// keptContainersOf tells whether the containers of an app are the ones compose would
// start as they are for prj, and why not otherwise. They are when each service has one
// stopped container of the project, created from the image the service names now, and
// the main service is the only one that waits for others.
//
// It is asked only for a project whose fingerprint matches, so what compose would bring
// up is what it brought up last: the image behind a tag is the one thing the fingerprint
// does not tell. The engine tells it: it lists a container under the name of its image
// only while that name is still the image the container was created from, and under the
// id of that image once the name was moved to another image or removed.
func keptContainersOf(prj *types.Project, containers []container.Summary) (*keptContainers, string) {
	byService := make(map[string]container.Summary, len(containers))
	for _, c := range containers {
		name := c.Labels[composeServiceLabel]
		if _, twice := byService[name]; twice {
			return nil, fmt.Sprintf("the service %s has more than one container", name)
		}
		if c.Labels[composeProjectLabel] != prj.Name {
			return nil, fmt.Sprintf("the container %s belongs to another project", c.ID)
		}
		if _, known := prj.Services[name]; !known {
			return nil, fmt.Sprintf("the container %s is of no service of the project", c.ID)
		}
		byService[name] = c
	}

	kept := &keptContainers{}
	mainName := ""
	for name, service := range prj.Services {
		c, found := byService[name]
		switch {
		case !found:
			return nil, fmt.Sprintf("the service %s has no container", name)
		case c.State != container.StateExited && c.State != container.StateCreated:
			return nil, fmt.Sprintf("the container of the service %s is %s", name, c.State)
		case service.Build != nil:
			return nil, fmt.Sprintf("the image of the service %s is built at every start", name)
		}
		if image := api.GetImageNameOrDefault(service, prj.Name); c.Image != image {
			return nil, fmt.Sprintf("the container of the service %s was not created from the image %s as it is now", name, image)
		}
		if !labeled(c.Labels, DockerAppMainLabel) {
			if len(service.DependsOn) > 0 {
				return nil, fmt.Sprintf("the service %s waits for other services", name)
			}
			continue
		}
		if mainName != "" {
			return nil, "the project has more than one main service"
		}
		mainName, kept.main = name, c
	}
	if mainName == "" {
		return nil, "the project has no main service"
	}

	for name, dependency := range prj.Services[mainName].DependsOn {
		switch dependency.Condition {
		case types.ServiceConditionStarted, types.ServiceConditionHealthy:
		default:
			return nil, fmt.Sprintf("the main service waits for the service %s to be %s", name, dependency.Condition)
		}
	}
	for name := range prj.Services {
		if name == mainName {
			continue
		}
		dependency := prj.Services[mainName].DependsOn[name]
		kept.dependencies = append(kept.dependencies, keptContainer{
			Summary:     byService[name],
			waitHealthy: dependency.Condition == types.ServiceConditionHealthy,
		})
	}
	return kept, ""
}

// startKeptContainers starts an app from the containers a stop left behind, with no work
// for compose, and tells whether it did. The caller makes sure the project is the one
// compose brought up last; the containers must be the ones compose would start as they
// are, see keptContainersOf.
//
// It reports false with no error whenever compose has to bring the project up instead:
// the containers are not fit, or one of them did not start, as when the network of a
// stopped app was given up. A dependency that turns unhealthy is an error, as it is for
// compose.
func startKeptContainers(ctx context.Context, docker dockerClient.APIClient, prj *types.Project, appPath *paths.Path) (bool, error) {
	began := time.Now()
	log := slog.With(slog.String("app", appPath.String()))

	containers, err := dockerhelper.Containers(ctx, docker, DockerAppPathLabel+"="+appPath.String())
	if err != nil {
		return false, err
	}
	kept, reason := keptContainersOf(prj, containers)
	if kept == nil {
		log.Info("the app is started by compose", slog.String("reason", reason))
		return false, nil
	}

	start := func(c container.Summary) bool {
		_, err := docker.ContainerStart(ctx, c.ID, dockerClient.ContainerStartOptions{})
		if err != nil && ctx.Err() == nil {
			log.Warn("the app is started by compose: a container did not start", slog.String("container", c.ID), slog.String("error", err.Error()))
		}
		return err == nil
	}
	for _, dependency := range kept.dependencies {
		if !start(dependency.Summary) {
			return false, ctx.Err()
		}
	}
	for _, dependency := range kept.dependencies {
		if !dependency.waitHealthy {
			continue
		}
		if err := waitContainerHealthy(ctx, docker, dependency.ID); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, fmt.Errorf("dependency failed to start: container %s %w", dependency.Labels[composeServiceLabel], err)
		}
	}
	if !start(kept.main) {
		return false, ctx.Err()
	}
	log.Info("the app is started from the containers its stop left", slog.Duration("elapsed", time.Since(began)))
	return true, nil
}

// waitContainerHealthy waits for the healthcheck of a started container to pass, as
// compose does for a service_healthy dependency. It fails when the container exits or
// its healthcheck fails.
func waitContainerHealthy(ctx context.Context, docker dockerClient.APIClient, id string) error {
	for {
		inspected, err := docker.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		state := inspected.Container.State
		switch {
		case state == nil:
			return errors.New("has no state")
		case !state.Running && !state.Restarting:
			return fmt.Errorf("exited with code %d", state.ExitCode)
		case state.Health == nil:
			return errors.New("has no healthcheck")
		case state.Health.Status == container.Healthy:
			return nil
		case state.Health.Status == container.Unhealthy:
			return errors.New("is unhealthy")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthPollInterval):
		}
	}
}
