// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// appDefinitionLabel holds the hash of the compose file a container was created from.
const appDefinitionLabel = "cc.arduino.app.definition"

// appContainerSpec is what the Docker API creates the container of an app from.
type appContainerSpec struct {
	config     *container.Config
	host       *container.HostConfig
	networking *network.NetworkingConfig
}

// upAppContainer brings the app container to its compose file through the Docker API, without the
// compose engine: an unchanged container is started when stopped, a changed one is created again.
// It returns the container ID.
func upAppContainer(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, c appContainer, cb func(StreamMessage)) (string, error) {
	data, err := c.composeFile(cfg).ReadFile()
	if err != nil {
		return "", fmt.Errorf("failed to read the app container compose file: %w", err)
	}
	sum := sha256.Sum256(data)
	definition := hex.EncodeToString(sum[:])

	containers, err := getAppContainers(ctx, docker, c.project)
	if err != nil {
		return "", err
	}
	if current, ok := containers[mainServiceName]; ok {
		switch {
		case current.Labels[appDefinitionLabel] != definition:
			slog.Debug("the app container definition changed, creating it again", slog.String("container", containerName(current)))
		case current.State == container.StateRunning:
			return current.ID, nil
		case current.State == container.StateCreated || current.State == container.StateExited:
			slog.Debug("starting the stopped app container", slog.String("container", containerName(current)))
			return current.ID, startAppContainer(ctx, docker, current.ID)
		}
		if err := removeContainers(ctx, docker, []container.Summary{current}); err != nil {
			return "", err
		}
	}

	spec, err := loadAppContainerSpec(ctx, cfg, c, definition)
	if err != nil {
		return "", err
	}
	id, err := createAppContainer(ctx, docker, c, spec, cb)
	if err != nil {
		return "", err
	}
	return id, startAppContainer(ctx, docker, id)
}

// createAppContainer creates the container, pulling its image when the board does not have it.
func createAppContainer(ctx context.Context, docker dockerClient.APIClient, c appContainer, spec appContainerSpec, cb func(StreamMessage)) (string, error) {
	options := dockerClient.ContainerCreateOptions{
		Name:             c.containerName(),
		Config:           spec.config,
		HostConfig:       spec.host,
		NetworkingConfig: spec.networking,
	}
	created, err := docker.ContainerCreate(ctx, options)
	// The network exists already: what is not found is the image
	if cerrdefs.IsNotFound(err) {
		image := spec.config.Image
		cb(StreamMessage{data: fmt.Sprintf("Pulling the app image %s", image)})
		if pullErr := dockerhelper.PullImages(ctx, docker, []string{image}, func(line string) { cb(StreamMessage{data: line}) }, nil); pullErr != nil {
			return "", fmt.Errorf("the app image %s is missing and could not be pulled: %w", image, pullErr)
		}
		created, err = docker.ContainerCreate(ctx, options)
	}
	if err != nil {
		return "", fmt.Errorf("failed to create the app container: %w", err)
	}
	slog.Debug("app container created", slog.String("container", c.containerName()))
	return created.ID, nil
}

func startAppContainer(ctx context.Context, docker dockerClient.APIClient, id string) error {
	if _, err := docker.ContainerStart(ctx, id, dockerClient.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start the app container: %w", err)
	}
	return nil
}

// loadAppContainerSpec reads the compose file of the app container as compose does, and turns
// its service into what the Docker API takes, with the labels compose puts on its containers.
func loadAppContainerSpec(ctx context.Context, cfg config.Configuration, c appContainer, definition string) (appContainerSpec, error) {
	file := c.composeFile(cfg)
	prj, err := loader.LoadWithContext(ctx,
		types.ConfigDetails{
			ConfigFiles: []types.ConfigFile{{Filename: file.String()}},
			WorkingDir:  file.Parent().String(),
			Environment: types.Mapping{},
		},
		func(o *loader.Options) { o.ResolvePaths = true },
	)
	if err != nil {
		return appContainerSpec{}, fmt.Errorf("failed to load %s: %w", file, err)
	}
	svc, ok := prj.Services[mainServiceName]
	if !ok {
		return appContainerSpec{}, fmt.Errorf("%s has no %s service", file, mainServiceName)
	}

	labels := map[string]string{
		api.ProjectLabel:         prj.Name,
		api.ServiceLabel:         mainServiceName,
		api.ContainerNumberLabel: "1",
		api.OneoffLabel:          "False",
		api.WorkingDirLabel:      file.Parent().String(),
		api.ConfigFilesLabel:     file.String(),
		// Compose lists only the containers carrying its config hash: the definition hash stands for it
		api.ConfigHashLabel: definition,
		api.VersionLabel:    api.ComposeVersion,
		appDefinitionLabel:  definition,
	}
	for k, v := range svc.Labels {
		labels[k] = v
	}
	env := make([]string, 0, len(svc.Environment))
	for k, v := range svc.Environment {
		if v != nil {
			env = append(env, k+"="+*v)
		}
	}
	mounts := make([]mount.Mount, 0, len(svc.Volumes))
	for _, v := range svc.Volumes {
		mounts = append(mounts, mount.Mount{Type: mount.Type(v.Type), Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
	}
	var stopTimeout *int
	if svc.StopGracePeriod != nil {
		seconds := int(time.Duration(*svc.StopGracePeriod).Seconds())
		stopTimeout = &seconds
	}
	host := &container.HostConfig{
		Init:          svc.Init,
		Mounts:        mounts,
		GroupAdd:      svc.GroupAdd,
		ExtraHosts:    svc.ExtraHosts.AsList(":"),
		NetworkMode:   container.NetworkMode(appsNetwork),
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		Resources:     container.Resources{DeviceCgroupRules: svc.DeviceCgroupRules},
	}
	if svc.Logging != nil {
		host.LogConfig = container.LogConfig{Type: svc.Logging.Driver, Config: svc.Logging.Options}
	}
	var aliases []string
	if n := svc.Networks["default"]; n != nil {
		aliases = n.Aliases
	}
	return appContainerSpec{
		config: &container.Config{
			Image:       svc.Image,
			User:        svc.User,
			Entrypoint:  svc.Entrypoint,
			WorkingDir:  svc.WorkingDir,
			Env:         env,
			Labels:      labels,
			StopTimeout: stopTimeout,
		},
		host:       host,
		networking: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{appsNetwork: {Aliases: aliases}}},
	}, nil
}
