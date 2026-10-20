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
	"net/netip"
	"slices"
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
// Docker binds the published ports while a container runs, so the containers of other apps holding
// the ports are stopped first; with park they are left alone and the container stays stopped. It
// returns the container ID and whether the container runs.
func upAppContainer(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, c appContainer, ports []int, park bool, cb func(StreamMessage)) (string, bool, error) {
	data, err := c.composeFile(cfg).ReadFile()
	if err != nil {
		return "", false, fmt.Errorf("failed to read the app container compose file: %w", err)
	}
	sum := sha256.Sum256(data)
	definition := hex.EncodeToString(sum[:])

	containers, err := getAppContainers(ctx, docker, c.project)
	if err != nil {
		return "", false, err
	}
	id := ""
	if current, ok := containers[mainServiceName]; ok {
		switch {
		case current.Labels[appDefinitionLabel] != definition:
			slog.Debug("the app container definition changed, creating it again", slog.String("container", containerName(current)))
			if err := removeContainers(ctx, docker, []container.Summary{current}); err != nil {
				return "", false, err
			}
		case current.State == container.StateRunning:
			return current.ID, true, nil
		case current.State == container.StateCreated || current.State == container.StateExited:
			id = current.ID
		default:
			if err := removeContainers(ctx, docker, []container.Summary{current}); err != nil {
				return "", false, err
			}
		}
	}
	if id == "" {
		spec, err := loadAppContainerSpec(ctx, cfg, c, definition)
		if err != nil {
			return "", false, err
		}
		if id, err = createAppContainer(ctx, docker, c, spec, cb); err != nil {
			return "", false, err
		}
	}

	holders, err := portHolders(ctx, docker, c, ports)
	if err != nil {
		return "", false, err
	}
	if park && len(holders) > 0 {
		slog.Debug("the app container waits stopped, its ports are held", slog.String("container", c.containerName()), slog.Any("ports", ports))
		return id, false, nil
	}
	if err := stopPortHolders(ctx, docker, holders, cb); err != nil {
		return "", false, err
	}
	slog.Debug("starting the app container", slog.String("container", c.containerName()))
	return id, true, startAppContainer(ctx, docker, id)
}

// portHolders are the running containers of other apps publishing a port of the app: Docker refuses
// to start a second container on a bound host port.
func portHolders(ctx context.Context, docker dockerClient.APIClient, c appContainer, ports []int) ([]container.Summary, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	containers, err := listAppMainContainers(ctx, docker)
	if err != nil {
		return nil, err
	}
	var holders []container.Summary
	for _, other := range containers {
		if other.State != container.StateRunning || containerName(other) == c.containerName() {
			continue
		}
		if slices.ContainsFunc(other.Ports, func(p container.PortSummary) bool { return slices.Contains(ports, int(p.PublicPort)) }) {
			holders = append(holders, other)
		}
	}
	return holders, nil
}

// stopPortHolders stops the containers so that their ports are free. Their apps stopped already:
// one app runs at a time and the start stops the previous one before the container of the next.
func stopPortHolders(ctx context.Context, docker dockerClient.APIClient, holders []container.Summary, cb func(StreamMessage)) error {
	for _, holder := range holders {
		cb(StreamMessage{data: fmt.Sprintf("Stopping the container %s, which holds the app ports", containerName(holder))})
		slog.Info("stopping an app container holding the ports", slog.String("container", containerName(holder)))
		if _, err := docker.ContainerStop(ctx, holder.ID, dockerClient.ContainerStopOptions{}); err != nil {
			return fmt.Errorf("failed to stop %s, which holds the app ports: %w", containerName(holder), err)
		}
	}
	return nil
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
	exposed, bindings, err := publishedPorts(svc.Ports)
	if err != nil {
		return appContainerSpec{}, fmt.Errorf("%s publishes an invalid port: %w", file, err)
	}
	host := &container.HostConfig{
		Init:          svc.Init,
		Mounts:        mounts,
		GroupAdd:      svc.GroupAdd,
		ExtraHosts:    svc.ExtraHosts.AsList(":"),
		NetworkMode:   container.NetworkMode(appsNetwork),
		PortBindings:  bindings,
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
			Image:        svc.Image,
			User:         svc.User,
			Entrypoint:   svc.Entrypoint,
			WorkingDir:   svc.WorkingDir,
			Env:          env,
			Labels:       labels,
			ExposedPorts: exposed,
			StopTimeout:  stopTimeout,
		},
		host:       host,
		networking: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{appsNetwork: {Aliases: aliases}}},
	}, nil
}

// publishedPorts turns the ports of a compose service into what the Docker API takes, as compose does.
func publishedPorts(ports []types.ServicePortConfig) (network.PortSet, network.PortMap, error) {
	exposed := network.PortSet{}
	bindings := network.PortMap{}
	for _, p := range ports {
		port, err := network.ParsePort(fmt.Sprintf("%d/%s", p.Target, p.Protocol))
		if err != nil {
			return nil, nil, err
		}
		var hostIP netip.Addr
		if p.HostIP != "" {
			if hostIP, err = netip.ParseAddr(p.HostIP); err != nil {
				return nil, nil, err
			}
		}
		exposed[port] = struct{}{}
		bindings[port] = append(bindings[port], network.PortBinding{HostIP: hostIP, HostPort: p.Published})
	}
	return exposed, bindings, nil
}
