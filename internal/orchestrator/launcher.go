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
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/types"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/modelsindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/servicesindex"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

// Each app runs in a warm container of its own, the main service of its compose project, whose
// supervisor forks the app on request; the sidecars of the project run with the app. The main
// container stays once the app stops: the next start of the app skips its warm-up. The container publishes the public ports of the app,
// which Docker binds while it runs: two containers publishing one port cannot run together, so the
// container of an app whose ports another app holds waits stopped, and a start stops the holder.
const (
	// launcherLabel marks the launcher shared by every app in the previous versions.
	launcherLabel = "cc.arduino.app.launcher"

	// appSocketDir is where an app container sees its runtime folder, which holds the control socket.
	appSocketDir = "/run/arduino-app-cli"
	// appMountDir is where an app container sees the app folder.
	appMountDir     = "/app"
	appReadyTimeout = 90 * time.Second

	launcherRequestTimeout = 2 * time.Second
	// A stop waits a 5 s grace before SIGKILL; a supervisor silent for longer is restarted with its container.
	launcherStopTimeout = 15 * time.Second
	// launcherPrepareTimeout bounds the install of the app dependencies, the supervisor is given it.
	launcherPrepareTimeout = 600 * time.Second
	// A start stops the running app, prepares the new one and forks it.
	launcherStartTimeout = launcherStopTimeout + launcherPrepareTimeout

	// maxKeyLength keeps the host name a DNS label and the socket path within the Unix limit.
	maxKeyLength = 56
)

// appContainer is the warm container of an app, named after its compose project.
type appContainer struct {
	project string
}

func appContainerOf(cfg config.Configuration, appPath *paths.Path) appContainer {
	return appContainer{project: composeProjectName(appPath, cfg.AppsDir())}
}

// dir holds the record of the project the containers were created from.
func (c appContainer) dir(cfg config.Configuration) *paths.Path {
	return appsStateDir(cfg).Join(c.project)
}

// runtimeDir is mounted in the container at appSocketDir.
func (c appContainer) runtimeDir(cfg config.Configuration) *paths.Path {
	return cfg.RuntimeDir().Join("apps", c.key())
}

// containerName is the name compose would give the container.
func (c appContainer) containerName() string {
	return c.project + "-" + mainServiceName + "-1"
}

// key is the project, cut and told apart by a hash when longer than maxKeyLength.
func (c appContainer) key() string {
	if len(c.project) <= maxKeyLength {
		return c.project
	}
	sum := sha256.Sum256([]byte(c.project))
	suffix := hex.EncodeToString(sum[:6])
	return strings.TrimRight(c.project[:maxKeyLength-len(suffix)-1], "-") + "-" + suffix
}

// appsStateDir holds the record of each app project and of the app that runs.
func appsStateDir(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("apps")
}

// appPorts are the public ports of an app: the defaults of its bricks, then the ports of app.yaml.
func appPorts(a app.ArduinoApp, bricksIndex *bricksindex.BricksIndex) []int {
	// A copy: WithAppBricks would change the index shared by the callers
	index := &bricksindex.BricksIndex{BuiltInBricks: bricksIndex.BuiltInBricks, AppBricks: a.LocalBricks}
	var ports []int
	for _, b := range a.Descriptor.Bricks {
		brick, found := index.FindBrickByID(b.ID)
		if !found {
			continue
		}
		for _, p := range brick.Ports {
			port, err := strconv.Atoi(p)
			if err != nil {
				slog.Warn("skipping invalid brick port", slog.String("brick_id", brick.ID), slog.String("port", p))
				continue
			}
			ports = append(ports, port)
		}
	}
	ports = append(ports, a.Descriptor.Ports...)
	slices.Sort(ports)
	return slices.Compact(ports)
}

// checkAppPorts tells whether the app container can publish the ports.
func checkAppPorts(ports []int) error {
	const maxPort = 65535
	for _, p := range ports {
		if p < 1 || p > maxPort {
			return fmt.Errorf("the app container cannot publish port %d: the app ports go from 1 to %d", p, maxPort)
		}
	}
	return nil
}

// boardEnvironment is what every app container is given: the host facts that hold for every app.
// The camera is a fact of the app start: a plugged camera must not recreate the container.
func boardEnvironment(ctx context.Context, plat platform.Platform, cfg config.Configuration) types.Mapping {
	envs := hostEnvironment(ctx, cfg.AppsDir(), cfg)
	delete(envs, "APP_HOME")
	delete(envs, "VIDEO_DEVICE")
	envs["BOARD_NAME"] = plat.BoardName
	return envs
}

// ensureAppContainer brings the containers of the app project to date, the main one running with its
// supervisor answering and, on a start, the sidecars running too. Containers created from the same
// project and an answer on the socket need no compose round trip. A start stops the containers of
// other apps holding the ports; a warm-up, park, creates the main container only and leaves it
// stopped when its ports are held, which it tells by returning false.
func ensureAppContainer(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	c appContainer,
	project appProject,
	park bool,
	cb func(StreamMessage),
) (time.Duration, bool, error) {
	started := time.Now()
	if err := c.runtimeDir(cfg).MkdirAll(); err != nil {
		return 0, false, fmt.Errorf("failed to create the app runtime folder: %w", err)
	}
	unchanged := c.recordedFingerprint(cfg) == project.fingerprint
	if unchanged {
		err := c.client(ctx, docker.Client(), cfg).WithTimeout(pingTimeout).Ping(ctx)
		if err == nil {
			if !park && len(project.services()) > 1 {
				if err := startSidecars(ctx, docker.Client(), c, project); err != nil {
					return 0, false, err
				}
			}
			elapsed := time.Since(started)
			slog.Debug("app container answers, up to date", slog.String("project", c.project), slog.Duration("elapsed", elapsed))
			return elapsed, true, nil
		}
		// Something else answers on the socket path: the container is restarted, its supervisor binds the socket anew
		if errors.Is(err, launcher.ErrPeer) {
			if err := restartAppContainer(ctx, docker.Client(), c, err.Error()); err != nil {
				return 0, false, err
			}
		}
	}

	containers, err := getAppContainers(ctx, docker.Client(), c.project)
	if err != nil {
		return 0, false, err
	}
	main, hasMain := containers[mainServiceName]
	holders, err := portHolders(ctx, docker.Client(), c, project.ports())
	if err != nil {
		return 0, false, err
	}
	line := func(l string) { cb(StreamMessage{data: l}) }
	// Compose recreates a changed container: the one listed above is gone
	recreated := false
	switch {
	case park:
		if !unchanged || !hasMain {
			slog.Debug("creating the app container", slog.String("project", c.project), slog.Bool("changed", !unchanged))
			if err := dockerhelper.ComposeCreate(ctx, docker, project.prj, []string{mainServiceName}, line); err != nil {
				return 0, false, fmt.Errorf("failed to create the app container: %w", err)
			}
			if err := c.recordFingerprint(cfg, project.fingerprint); err != nil {
				return 0, false, err
			}
			recreated = true
		}
		if len(holders) > 0 {
			slog.Debug("the app container waits stopped, its ports are held", slog.String("container", c.containerName()), slog.Any("ports", project.ports()))
			return time.Since(started), false, nil
		}
		if err := startStoppedContainers(ctx, docker.Client(), c, mainServiceName); err != nil {
			return 0, false, err
		}
	case !unchanged || !containers.cover(project.services()):
		slog.Debug("bringing the app project up", slog.String("project", c.project), slog.Bool("changed", !unchanged))
		cb(StreamMessage{data: "Starting the app containers"})
		if err := stopPortHolders(ctx, docker.Client(), holders, cb); err != nil {
			return 0, false, err
		}
		if err := dockerhelper.ComposeUp(ctx, docker, project.prj, line); err != nil {
			return 0, false, err
		}
		if err := c.recordFingerprint(cfg, project.fingerprint); err != nil {
			return 0, false, err
		}
		recreated = true
	default:
		if err := stopPortHolders(ctx, docker.Client(), holders, cb); err != nil {
			return 0, false, err
		}
		if err := startStoppedContainers(ctx, docker.Client(), c, project.services()...); err != nil {
			return 0, false, err
		}
	}
	if recreated || !hasMain {
		if containers, err = getAppContainers(ctx, docker.Client(), c.project); err != nil {
			return 0, false, err
		}
		if main, hasMain = containers[mainServiceName]; !hasMain {
			return 0, false, errors.New("the app container was not created")
		}
	}
	if err := waitAppContainerReady(ctx, docker.Client(), c.client(ctx, docker.Client(), cfg), main.ID); err != nil {
		return 0, false, err
	}
	if !park {
		if err := waitSidecarsReady(ctx, docker.Client(), c, project); err != nil {
			return 0, false, err
		}
	}
	elapsed := time.Since(started)
	slog.Info("app container ready", slog.String("project", c.project), slog.Duration("elapsed", elapsed), slog.Bool("changed", !unchanged))
	return elapsed, true, nil
}

// startSidecars starts the sidecars of the app that are not running and waits until they are ready:
// they stop with the app, the main container does not.
func startSidecars(ctx context.Context, docker dockerClient.APIClient, c appContainer, project appProject) error {
	sidecars := slices.DeleteFunc(project.services(), func(s string) bool { return s == mainServiceName })
	if err := startStoppedContainers(ctx, docker, c, sidecars...); err != nil {
		return err
	}
	return waitSidecarsReady(ctx, docker, c, project)
}

// waitSidecarsReady waits for the sidecars the way compose waits for the dependencies of the main
// service: healthy when the brick declares a healthcheck, running otherwise. The app is forked
// only then, as it was started only then.
func waitSidecarsReady(ctx context.Context, docker dockerClient.APIClient, c appContainer, project appProject) error {
	if len(project.services()) == 1 {
		return nil
	}
	containers, err := getAppContainers(ctx, docker, c.project)
	if err != nil {
		return err
	}
	var errs []error
	var wg sync.WaitGroup
	var mu sync.Mutex
	for name, svc := range project.prj.Services {
		if name == mainServiceName {
			continue
		}
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

// startStoppedContainers starts the containers of the services that exist and do not run.
func startStoppedContainers(ctx context.Context, docker dockerClient.APIClient, c appContainer, services ...string) error {
	containers, err := getAppContainers(ctx, docker, c.project)
	if err != nil {
		return err
	}
	stopped := make(appContainers)
	for _, service := range services {
		if ctr, ok := containers[service]; ok && ctr.State != container.StateRunning {
			stopped[service] = ctr
		}
	}
	return startAppContainers(ctx, docker, stopped, func(StreamMessage) {})
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

// pingTimeout bounds one ping of a supervisor, a local socket round trip.
const pingTimeout = 500 * time.Millisecond

// waitAppContainerReady polls ping until the preload is over. It fails early when the container
// stops, as an image without the serve mode does.
func waitAppContainerReady(ctx context.Context, docker dockerClient.APIClient, client *launcher.Client, id string) error {
	client = client.WithTimeout(pingTimeout)
	deadline := time.Now().Add(appReadyTimeout)
	for {
		err := client.Ping(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, launcher.ErrPeer) {
			return err
		}
		inspected, err := docker.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("failed to inspect the app container: %w", err)
		}
		info := inspected.Container
		if info.State == nil || !info.State.Running || info.State.Restarting {
			return fmt.Errorf("the app container stopped, see `docker logs %s`", strings.TrimPrefix(info.Name, "/"))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the app container did not answer within %s", appReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// isMainContainer tells the warm container of an app from its sidecars.
func isMainContainer(c container.Summary) bool {
	return c.Labels[DockerAppMainLabel] == "true"
}

// listAppMainContainers lists the app containers, running or not, the sidecars left out.
func listAppMainContainers(ctx context.Context, docker dockerClient.APIClient) ([]container.Summary, error) {
	return dockerhelper.Containers(ctx, docker, DockerAppMainLabel+"=true")
}

// StopAllApps stops the app of every running app container and the sidecars, leaving the containers
// warm. It removes the launcher an older version shared between the apps.
func StopAllApps(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration) error {
	containers, err := listAppMainContainers(ctx, docker)
	if err != nil {
		return err
	}
	running := slices.DeleteFunc(containers, func(c container.Summary) bool {
		return c.State != container.StateRunning || c.Labels[composeProjectLabel] == ""
	})
	err = forEachContainer(running, func(c container.Summary) error {
		return stopAppChild(ctx, docker, cfg, appContainer{project: c.Labels[composeProjectLabel]})
	})
	return errors.Join(err, clearWantedApp(cfg, nil), stopAllSidecars(ctx, docker), removeSharedLauncher(ctx, docker))
}

// stopAllSidecars stops every running sidecar of every app: the sidecars run with their app only.
func stopAllSidecars(ctx context.Context, docker dockerClient.APIClient) error {
	containers, err := dockerhelper.Containers(ctx, docker, DockerAppLabel+"=true")
	if err != nil {
		return err
	}
	sidecars := slices.DeleteFunc(containers, isMainContainer)
	return stopAppContainers(ctx, docker, byService(sidecars), DefaultDockerStopTimeoutSeconds, func(StreamMessage) {})
}

// removeSharedSidecars removes the sidecars project and the network the previous layout shared between the apps.
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

// removeSharedLauncher removes the launcher of the previous versions, which would keep its socket and mounts.
func removeSharedLauncher(ctx context.Context, docker dockerClient.APIClient) error {
	shared, err := dockerhelper.Containers(ctx, docker, launcherLabel+"=true")
	if err != nil || len(shared) == 0 {
		return err
	}
	slog.Info("removing the launcher shared by the apps in the previous versions")
	return removeContainers(ctx, docker, shared)
}

// stopAppChild stops the app the container runs. A stop the supervisor does not confirm, or that leaves any
// process behind, ends with a restart of the container: the app cannot hold on by tampering with its socket.
func stopAppChild(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, c appContainer) error {
	resp, err := c.client(ctx, docker, cfg).WithTimeout(launcherStopTimeout).Stop(ctx)
	var launcherErr *launcher.Error
	switch {
	case errors.As(err, &launcherErr):
		return fmt.Errorf("failed to stop the app: %w", err)
	case err != nil:
		return restartAppContainer(ctx, docker, c, "the app supervisor did not answer the stop: "+err.Error())
	}
	for _, s := range resp.Stopped {
		slog.Debug("app stopped", slog.String("project", c.project), slog.Int("pid", s.PID), slog.Any("exit_code", s.ExitCode), slog.Any("signal", s.Signal))
	}
	leftovers, err := c.leftovers(ctx, docker)
	if err != nil {
		slog.Debug("cannot check the app container after the stop", slog.String("project", c.project), slog.String("error", err.Error()))
		return nil
	}
	if leftovers {
		return restartAppContainer(ctx, docker, c, "processes left in the container after the stop")
	}
	return nil
}

// removeAppContainers removes app containers, running or not, and their runtime folders.
func removeAppContainers(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, containers []container.Summary) error {
	return forEachContainer(containers, func(ctr container.Summary) error {
		if err := removeContainers(ctx, docker, []container.Summary{ctr}); err != nil {
			return err
		}
		// Without project there is no runtime folder of its own: the parent holds every other one
		if project := ctr.Labels[composeProjectLabel]; project != "" {
			return appContainer{project: project}.runtimeDir(cfg).RemoveAll()
		}
		return nil
	})
}

// destroyAppContainer removes the project of the app, containers and network, with its runtime folder and its record.
func destroyAppContainer(ctx context.Context, docker command.Cli, cfg config.Configuration, appPath *paths.Path) error {
	return destroyAppProject(ctx, docker, cfg, appContainerOf(cfg, appPath))
}

func destroyAppProject(ctx context.Context, docker command.Cli, cfg config.Configuration, c appContainer) error {
	if err := dockerhelper.ComposeDown(ctx, docker, c.project, func(string) {}); err != nil {
		return fmt.Errorf("failed to remove the containers of %s: %w", c.project, err)
	}
	return errors.Join(c.runtimeDir(cfg).RemoveAll(), c.dir(cfg).RemoveAll())
}

// wantedApp is the app that should be running, and when it was started: a missing child means it died.
type wantedApp struct {
	App       *paths.Path
	StartedAt time.Time
}

type wantedAppRecord struct {
	App       string    `json:"app"`
	StartedAt time.Time `json:"started_at"`
}

func wantedAppFile(cfg config.Configuration) *paths.Path {
	return appsStateDir(cfg).Join("wanted.json")
}

func readWantedApp(cfg config.Configuration) *wantedApp {
	data, err := wantedAppFile(cfg).ReadFile()
	if err != nil {
		return nil
	}
	var record wantedAppRecord
	if err := json.Unmarshal(data, &record); err != nil || record.App == "" {
		return nil
	}
	return &wantedApp{App: paths.New(record.App), StartedAt: record.StartedAt}
}

func setWantedApp(cfg config.Configuration, appPath *paths.Path, startedAt time.Time) error {
	data, err := json.Marshal(wantedAppRecord{App: appPath.String(), StartedAt: startedAt})
	if err != nil {
		return err
	}
	file := wantedAppFile(cfg)
	if err := file.Parent().MkdirAll(); err != nil {
		return err
	}
	return fatomic.WriteFile(file.String(), data, 0o644)
}

// clearWantedApp forgets the wanted app when it is appPath, or whatever it is when nil.
func clearWantedApp(cfg config.Configuration, appPath *paths.Path) error {
	if wanted := readWantedApp(cfg); wanted == nil || (appPath != nil && !wanted.App.EqualsTo(appPath)) {
		return nil
	}
	if err := wantedAppFile(cfg).Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// removeStaleAppContainers removes the app containers of another image than the one of the
// python image tag, so that the next start creates them again on the new one.
func removeStaleAppContainers(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration) error {
	current, err := docker.ImageInspect(ctx, cfg.PythonImage)
	if cerrdefs.IsNotFound(err) {
		slog.Debug("the python image is missing, the app containers are left as they are", slog.String("image", cfg.PythonImage))
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to inspect the python image: %w", err)
	}
	containers, err := listAppMainContainers(ctx, docker)
	if err != nil {
		return err
	}
	stale := slices.DeleteFunc(containers, func(c container.Summary) bool { return c.ImageID == current.ID })
	for _, c := range stale {
		slog.Info("removing an app container of an older image", slog.String("container", containerName(c)))
	}
	return removeAppContainers(ctx, docker, cfg, stale)
}

// warmApps are the apps kept warm from boot: every user app and installed release, not the examples.
func warmApps(cfg config.Configuration) paths.PathList {
	appPaths, err := app.FindAppsInFolders(paths.PathList{cfg.AppsDir(), cfg.ReleasesDir()})
	if err != nil {
		slog.Warn("some apps were not listed for the warm-up", slog.String("error", err.Error()))
	}
	return appPaths
}

// orphanAppContainers are the app containers whose app folder is gone; one without path label is left alone.
func orphanAppContainers(containers []container.Summary) []container.Summary {
	return slices.DeleteFunc(slices.Clone(containers), func(c container.Summary) bool {
		appPath := c.Labels[DockerAppPathLabel]
		if appPath == "" {
			return true
		}
		isDir, _ := paths.New(appPath).IsDirCheck()
		return isDir
	})
}

// removeOrphanAppContainers removes the projects of the apps deleted outside the daemon.
func removeOrphanAppContainers(ctx context.Context, docker command.Cli, cfg config.Configuration) error {
	containers, err := listAppMainContainers(ctx, docker.Client())
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range orphanAppContainers(containers) {
		slog.Info("removing the containers of a deleted app", slog.String("container", containerName(c)))
		errs = append(errs, destroyAppProject(ctx, docker, cfg, appContainer{project: c.Labels[composeProjectLabel]}))
	}
	return errors.Join(errs...)
}

// prewarmPause separates two warm-ups: a start waiting for the apps lock takes it in between.
const prewarmPause = 200 * time.Millisecond

// PrewarmApps creates the main container of every app of the board and starts it, without starting
// the app, one at a time and never ahead of a start: a container the user asks for meanwhile comes
// up on the start path. The sidecars are created by the first start. An app whose ports a running
// container holds gets its container created and left stopped.
func PrewarmApps(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	platform platform.Platform,
	provisioner *Provision,
	bricksIndex *bricksindex.BricksIndex,
	servicesIndex *servicesindex.ServicesIndex,
	modelsIndex *modelsindex.ModelsIndex,
) {
	var running *paths.Path
	if wanted := readWantedApp(cfg); wanted != nil {
		running = wanted.App
	}
	for i, appPath := range warmApps(cfg) {
		if running != nil && running.EqualsTo(appPath) {
			continue
		}
		if i > 0 {
			time.Sleep(prewarmPause)
		}
		a, err := app.Load(appPath)
		if err != nil || a.MainPythonFile == nil {
			slog.Debug("skipping the warm-up of an app without python or invalid", slog.String("app", appPath.String()))
			continue
		}
		err = withAppsLock(ctx, cfg, func() error {
			// Resolved as a start resolves it: a release holds its files already
			appEnv := appEnvironment(ctx, a, bricksIndex, modelsIndex, platform)
			if _, isRelease := a.GetRelease(); !isRelease {
				if err := provisioner.Resolve(&a, a.ProvisioningStateDir(), bricksIndex, servicesIndex, cfg, appEnv, platform); err != nil {
					return err
				}
			}
			project, err := renderAppProject(ctx, provisioner, &a, cfg, platform, appEnv, bricksIndex)
			if err != nil {
				return err
			}
			elapsed, warm, err := ensureAppContainer(ctx, docker, cfg, appContainerOf(cfg, appPath), project, true, func(StreamMessage) {})
			slog.Debug("app container warmed up", slog.String("app", appPath.String()), slog.Bool("warm", warm), slog.Duration("elapsed", elapsed))
			return err
		})
		if err != nil {
			slog.Debug("failed to warm up an app container", slog.String("app", appPath.String()), slog.String("error", err.Error()))
		}
	}
}

// EnsureRuntime removes the app containers of an older image or of a deleted app, and what the
// previous layout left: the shared sidecars project and the shared network.
func EnsureRuntime(ctx context.Context, docker command.Cli, cfg config.Configuration) (time.Duration, error) {
	started := time.Now()
	err := withAppsLock(ctx, cfg, func() error {
		if err := removeStaleAppContainers(ctx, docker.Client(), cfg); err != nil {
			slog.Warn("failed to remove the app containers of an older image", slog.String("error", err.Error()))
		}
		if err := removeOrphanAppContainers(ctx, docker, cfg); err != nil {
			slog.Warn("failed to remove the containers of deleted apps", slog.String("error", err.Error()))
		}
		removeSharedSidecars(ctx, docker)
		return nil
	})
	return time.Since(started), err
}
