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
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
)

const (
	// DefaultDockerStopTimeoutSeconds is what an app container is given to stop before it is killed.
	DefaultDockerStopTimeoutSeconds = 5

	composeServiceLabel = "com.docker.compose.service"
	// mainServiceName is the app in the logs, and the service of the app container an older CLI ran
	mainServiceName = "main"

	// How long a sidecar may take to run before the start fails.
	sidecarReadyTimeout = 60 * time.Second
	readyPollInterval   = 100 * time.Millisecond
)

// appContainers are the containers compose created for an app, keyed by service name.
type appContainers map[string]container.Summary

// getAppContainers lists the containers of the app compose project, running or not.
func getAppContainers(ctx context.Context, docker dockerClient.APIClient, project string) (appContainers, error) {
	containers, err := docker.ContainerList(ctx, dockerClient.ContainerListOptions{
		All:     true,
		Filters: make(dockerClient.Filters).Add("label", composeProjectLabel+"="+project),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list the app containers: %w", err)
	}
	return byService(containers.Items), nil
}

// cover tells whether every service has a container.
func (c appContainers) cover(services []string) bool {
	for _, s := range services {
		if _, ok := c[s]; !ok {
			return false
		}
	}
	return true
}

// notRunning are the containers that do not run.
func (c appContainers) notRunning() appContainers {
	result := make(appContainers)
	for service, ctr := range c {
		if ctr.State != container.StateRunning {
			result[service] = ctr
		}
	}
	return result
}

func byService(containers []container.Summary) appContainers {
	result := make(appContainers, len(containers))
	for _, c := range containers {
		result[c.Labels[composeServiceLabel]] = c
	}
	return result
}

// startAppContainers starts the existing sidecars of an app together, each waited until it runs.
func startAppContainers(ctx context.Context, docker dockerClient.APIClient, containers appContainers, cb func(StreamMessage)) error {
	return forEachContainer(slices.Collect(maps.Values(containers)), func(c container.Summary) error {
		cb(StreamMessage{data: fmt.Sprintf("Container %s Starting", containerName(c))})
		if _, err := docker.ContainerStart(ctx, c.ID, dockerClient.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("failed to start %s: %w", containerName(c), err)
		}
		if err := waitContainerRunning(ctx, docker, c.ID); err != nil {
			return fmt.Errorf("%s did not start: %w", containerName(c), err)
		}
		cb(StreamMessage{data: fmt.Sprintf("Container %s Started", containerName(c))})
		return nil
	})
}

// stopAppContainers stops the running sidecars of an app at once.
func stopAppContainers(ctx context.Context, docker dockerClient.APIClient, containers appContainers, timeoutSeconds int, cb func(StreamMessage)) error {
	return forEachContainer(slices.Collect(maps.Values(containers)), func(c container.Summary) error {
		if c.State != container.StateRunning {
			return nil
		}
		cb(StreamMessage{data: fmt.Sprintf("Container %s Stopping", containerName(c))})
		if _, err := docker.ContainerStop(ctx, c.ID, dockerClient.ContainerStopOptions{Timeout: &timeoutSeconds}); err != nil {
			return fmt.Errorf("failed to stop %s: %w", containerName(c), err)
		}
		cb(StreamMessage{data: fmt.Sprintf("Container %s Stopped", containerName(c))})
		return nil
	})
}

// removeContainers stops and removes containers, running or not. The stop gives a sidecar the
// grace a compose down gives it, a database flushes its writes.
func removeContainers(ctx context.Context, docker dockerClient.APIClient, containers []container.Summary) error {
	timeout := DefaultDockerStopTimeoutSeconds
	return forEachContainer(containers, func(c container.Summary) error {
		slog.Debug("removing container", slog.String("container", containerName(c)))
		if c.State == container.StateRunning || c.State == container.StateRestarting {
			if _, err := docker.ContainerStop(ctx, c.ID, dockerClient.ContainerStopOptions{Timeout: &timeout}); err != nil {
				return fmt.Errorf("failed to stop %s: %w", containerName(c), err)
			}
		}
		if _, err := docker.ContainerRemove(ctx, c.ID, dockerClient.ContainerRemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("failed to remove %s: %w", containerName(c), err)
		}
		return nil
	})
}

// waitContainerRunning returns once the container runs, when Docker resolves its name on its
// networks. It fails when the container exits meanwhile.
func waitContainerRunning(ctx context.Context, docker dockerClient.APIClient, id string) error {
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
		case info.State.Running && !info.State.Restarting:
			return nil
		case !info.State.Running && !info.State.Restarting && info.State.Status != container.StateCreated:
			return fmt.Errorf("the container %s with code %d", info.State.Status, info.State.ExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still %s after %s", info.State.Status, sidecarReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
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

// waitContainersRunning waits for every container to run, see waitContainerRunning.
func waitContainersRunning(ctx context.Context, docker dockerClient.APIClient, containers appContainers) error {
	return forEachContainer(slices.Collect(maps.Values(containers)), func(c container.Summary) error {
		if err := waitContainerRunning(ctx, docker, c.ID); err != nil {
			return fmt.Errorf("%s is not running: %w", containerName(c), err)
		}
		return nil
	})
}

// forEachContainer runs fn on every container concurrently and returns the first error.
func forEachContainer(containers []container.Summary, fn func(container.Summary) error) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, c := range containers {
		wg.Add(1)
		go func(c container.Summary) {
			defer wg.Done()
			if err := fn(c); err != nil {
				slog.Debug("container operation failed", slog.String("container", containerName(c)), slog.String("error", err.Error()))
				mu.Lock()
				firstErr = errors.Join(firstErr, err)
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return firstErr
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return c.Names[0][1:]
	}
	return c.ID[:12]
}
