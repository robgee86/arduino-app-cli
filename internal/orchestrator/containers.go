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
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
)

const (
	// DefaultDockerStopTimeoutSeconds is what an app container is given to stop before it is killed.
	DefaultDockerStopTimeoutSeconds = 5

	composeServiceLabel = "com.docker.compose.service"
	mainServiceName     = "main"

	// How long a sidecar may take to become healthy before the start fails: the
	// same order of magnitude as the healthcheck retries compose would honor.
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
	result := make(appContainers, len(containers.Items))
	for _, c := range containers.Items {
		result[c.Labels[composeServiceLabel]] = c
	}
	return result, nil
}

// HasMain tells whether the app was created before, i.e. a start can reuse its containers.
func (c appContainers) HasMain() bool {
	_, ok := c[mainServiceName]
	return ok
}

func (c appContainers) sidecars() []container.Summary {
	result := make([]container.Summary, 0, len(c))
	for name, summary := range c {
		if name != mainServiceName {
			result = append(result, summary)
		}
	}
	return result
}

// startAppContainers starts the existing containers of an app: the sidecars together,
// each waited until healthy, then the main one, as compose does for depends_on.
func startAppContainers(ctx context.Context, docker dockerClient.APIClient, containers appContainers, cb func(StreamMessage)) error {
	if err := forEachContainer(containers.sidecars(), func(c container.Summary) error {
		cb(StreamMessage{data: fmt.Sprintf("Container %s Starting", containerName(c))})
		if _, err := docker.ContainerStart(ctx, c.ID, dockerClient.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("failed to start %s: %w", containerName(c), err)
		}
		if err := waitContainerReady(ctx, docker, c.ID); err != nil {
			return fmt.Errorf("%s did not become ready: %w", containerName(c), err)
		}
		cb(StreamMessage{data: fmt.Sprintf("Container %s Started", containerName(c))})
		return nil
	}); err != nil {
		return err
	}

	main := containers[mainServiceName]
	cb(StreamMessage{data: fmt.Sprintf("Container %s Starting", containerName(main))})
	if _, err := docker.ContainerStart(ctx, main.ID, dockerClient.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start %s: %w", containerName(main), err)
	}
	cb(StreamMessage{data: fmt.Sprintf("Container %s Started", containerName(main))})
	return nil
}

// stopAppContainers stops the main container first, so the app releases its peripherals
// and sidecar connections while they are still there, then every sidecar at once.
func stopAppContainers(ctx context.Context, docker dockerClient.APIClient, containers appContainers, timeoutSeconds int, cb func(StreamMessage)) error {
	stop := func(c container.Summary) error {
		if c.State != container.StateRunning {
			return nil
		}
		cb(StreamMessage{data: fmt.Sprintf("Container %s Stopping", containerName(c))})
		if _, err := docker.ContainerStop(ctx, c.ID, dockerClient.ContainerStopOptions{Timeout: &timeoutSeconds}); err != nil {
			return fmt.Errorf("failed to stop %s: %w", containerName(c), err)
		}
		cb(StreamMessage{data: fmt.Sprintf("Container %s Stopped", containerName(c))})
		return nil
	}
	if main, ok := containers[mainServiceName]; ok {
		if err := stop(main); err != nil {
			return err
		}
	}
	return forEachContainer(containers.sidecars(), stop)
}

// waitContainerReady returns once a container with a healthcheck is healthy, or as soon
// as it runs when it has none. It fails when the container stops meanwhile.
func waitContainerReady(ctx context.Context, docker dockerClient.APIClient, id string) error {
	deadline := time.Now().Add(sidecarReadyTimeout)
	for {
		inspected, err := docker.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		info := inspected.Container
		switch {
		case info.State == nil || !info.State.Running:
			return errors.New("the container is not running")
		case info.State.Health == nil, info.State.Health.Status == container.Healthy:
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
