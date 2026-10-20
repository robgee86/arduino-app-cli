// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bytes"
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
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	yaml "github.com/goccy/go-yaml"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

// Each app runs in a warm container of its own, the compose project of the app with one service,
// main, whose supervisor forks the app on request. The container stays once the app stops: the
// next start of the app skips its warm-up.
const (
	// launcherLabel marks the launcher shared by every app in the previous versions.
	launcherLabel = "cc.arduino.app.launcher"

	// appsNetwork is shared by the app containers, the sidecars and the ingress, which reach each other by name.
	appsNetwork = "arduino-apps"
	// appSocketDir is where an app container sees its runtime folder, which holds the control socket.
	appSocketDir = "/run/arduino-app-cli"
	// appMountDir is where an app container sees the app folder.
	appMountDir     = "/app"
	appReadyTimeout = 90 * time.Second

	launcherRequestTimeout = 2 * time.Second
	// A stop waits a 5 s grace before SIGKILL.
	launcherStopTimeout = 60 * time.Second
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

// dir holds the compose file of the container.
func (c appContainer) dir(cfg config.Configuration) *paths.Path {
	return appsStateDir(cfg).Join(c.project)
}

func (c appContainer) composeFile(cfg config.Configuration) *paths.Path {
	return c.dir(cfg).Join("compose.yaml")
}

// runtimeDir is mounted in the container at appSocketDir.
func (c appContainer) runtimeDir(cfg config.Configuration) *paths.Path {
	return cfg.RuntimeDir().Join("apps", c.key())
}

func (c appContainer) client(cfg config.Configuration) *launcher.Client {
	return launcher.New(c.runtimeDir(cfg).Join(launcher.SocketName).String(), launcherRequestTimeout)
}

// containerName is the name compose would give the container.
func (c appContainer) containerName() string {
	return c.project + "-" + mainServiceName + "-1"
}

// host is the name the ingress reaches the container by, its compose name for a short project.
func (c appContainer) host() string {
	return c.key() + "-" + mainServiceName + "-1"
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

// appsStateDir holds the compose file of each app container and the records of the apps that run.
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

// checkAppPorts tells whether the ingress can publish the ports.
func checkAppPorts(ports []int) error {
	const maxPort = 65535
	for _, p := range ports {
		if p < 1 || p > maxPort {
			return fmt.Errorf("the ingress cannot publish port %d: the app ports go from 1 to %d", p, maxPort)
		}
	}
	return nil
}

// boardAppRoots are the folders holding the apps of the board, the installed releases included.
func boardAppRoots(cfg config.Configuration) paths.PathList {
	return paths.PathList{cfg.AppsDir(), cfg.ExamplesBaseDir(), cfg.ReleasesDir()}
}

// boardAppPorts are the public ports of every app of the board. An app that fails to load has none.
func boardAppPorts(cfg config.Configuration, bricksIndex *bricksindex.BricksIndex) []int {
	appPaths, err := app.FindAppsInFolders(boardAppRoots(cfg))
	if err != nil {
		slog.Warn("some apps were not listed for the ingress ports", slog.String("error", err.Error()))
	}
	var ports []int
	for _, appPath := range appPaths {
		a, err := app.Load(appPath)
		if err != nil {
			slog.Debug("skipping the ports of an invalid app", slog.String("path", appPath.String()), slog.String("error", err.Error()))
			continue
		}
		ports = append(ports, appPorts(a, bricksIndex)...)
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

// generateAppComposeFile writes the compose project of the app container, rendered on this board,
// and tells whether it changed. It holds nothing an app edits: the app environment goes with each start.
func generateAppComposeFile(
	c appContainer,
	appPath *paths.Path,
	pythonImage string,
	cfg config.Configuration,
	boardEnvs types.Mapping,
	platform platform.Platform,
) (bool, error) {
	envs := boardEnvs.Clone()
	envs["APP_LAUNCHER_PREPARE_TIMEOUT_S"] = strconv.Itoa(int(launcherPrepareTimeout.Seconds()))
	if cfg.LauncherPreload != "" {
		envs["APP_LAUNCHER_PRELOAD"] = cfg.LauncherPreload // Part of the compose file: changing it recreates the container
	}

	runtime, err := newPythonRuntime(cfg, platform)
	if err != nil {
		return false, err
	}
	svc := runtime.service(pythonImage,
		volume{Type: "bind", Source: appPath.String(), Target: appMountDir},
		volume{Type: "bind", Source: c.runtimeDir(cfg).String(), Target: appSocketDir},
	)
	svc.Entrypoint = "/run.sh serve"
	svc.WorkingDir = appMountDir
	svc.Restart = "no"
	svc.StopGracePeriod = "10s"
	svc.Labels = map[string]string{
		DockerAppLabel:     "true",
		DockerAppMainLabel: "true",
		DockerAppPathLabel: appPath.String(),
	}
	svc.Environment = envs
	svc.Networks = map[string]serviceNetwork{"default": {Aliases: []string{c.host()}}}

	compose := struct {
		Name     string                    `yaml:"name"`
		Services map[string]service        `yaml:"services"`
		Networks map[string]composeNetwork `yaml:"networks"`
	}{
		Name:     c.project,
		Services: map[string]service{mainServiceName: svc},
		Networks: map[string]composeNetwork{"default": {Name: appsNetwork, External: true}},
	}
	template, err := yaml.Marshal(compose)
	if err != nil {
		return false, err
	}
	// The host devices and groups are answered here, the board the container runs on
	data, err := renderComposeTemplate(template)
	if err != nil {
		return false, fmt.Errorf("failed to render the app container compose file: %w", err)
	}
	// compose interpolates the file: a $ of an app path or of the board environment is escaped
	data = bytes.ReplaceAll(data, []byte("$"), []byte("$$"))

	changed, err := writeFileIfChanged(c.composeFile(cfg), data)
	if err != nil {
		return false, fmt.Errorf("failed to write the app container compose file: %w", err)
	}
	return changed, nil
}

// writeFileIfChanged writes data atomically unless the file already holds it, and tells whether it wrote.
func writeFileIfChanged(file *paths.Path, data []byte) (bool, error) {
	if before, err := file.ReadFile(); err == nil && bytes.Equal(before, data) {
		return false, nil
	}
	if err := file.Parent().MkdirAll(); err != nil {
		return false, err
	}
	if err := fatomic.WriteFile(file.String(), data, 0o644); err != nil {
		return false, err
	}
	return true, nil
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

// ensureAppContainer brings the container of the app up to date and running, and waits until its
// supervisor answers. An unchanged compose file and an answer on the socket need no Docker round trip.
func ensureAppContainer(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	platform platform.Platform,
	c appContainer,
	appPath *paths.Path,
	cb func(StreamMessage),
) (time.Duration, error) {
	started := time.Now()
	if err := c.runtimeDir(cfg).MkdirAll(); err != nil {
		return 0, fmt.Errorf("failed to create the app runtime folder: %w", err)
	}
	changed, err := generateAppComposeFile(c, appPath, cfg.PythonImage, cfg, boardEnvironment(ctx, platform, cfg), platform)
	if err != nil {
		return 0, err
	}
	if !changed {
		if err := c.client(cfg).WithTimeout(pingTimeout).Ping(ctx); err == nil {
			elapsed := time.Since(started)
			slog.Debug("app container answers, up to date", slog.String("project", c.project), slog.Duration("elapsed", elapsed))
			return elapsed, nil
		}
	}

	slog.Debug("bringing the app container up", slog.String("project", c.project), slog.Bool("changed", changed))
	cb(StreamMessage{data: "Starting the app container"})
	if err := ensureAppsNetwork(ctx, docker.Client()); err != nil {
		return 0, err
	}
	id, err := upAppContainer(ctx, docker.Client(), cfg, c, cb)
	if err != nil {
		return 0, err
	}
	if err := waitAppContainerReady(ctx, docker.Client(), c.client(cfg), id); err != nil {
		return 0, err
	}
	elapsed := time.Since(started)
	slog.Info("app container ready", slog.String("project", c.project), slog.Duration("elapsed", elapsed), slog.Bool("changed", changed))
	return elapsed, nil
}

// pingTimeout bounds one ping of a supervisor, a local socket round trip.
const pingTimeout = 500 * time.Millisecond

// waitAppContainerReady polls ping until the preload is over. It fails early when the container
// stops, as an image without the serve mode does.
func waitAppContainerReady(ctx context.Context, docker dockerClient.APIClient, client *launcher.Client, id string) error {
	client = client.WithTimeout(pingTimeout)
	deadline := time.Now().Add(appReadyTimeout)
	for {
		if err := client.Ping(ctx); err == nil {
			return nil
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

// ensureAppsNetwork creates the network shared by the apps, the sidecars and the ingress when missing.
func ensureAppsNetwork(ctx context.Context, docker dockerClient.APIClient) error {
	networks, err := docker.NetworkList(ctx, dockerClient.NetworkListOptions{
		Filters: make(dockerClient.Filters).Add("name", appsNetwork),
	})
	if err != nil {
		return fmt.Errorf("failed to list the networks: %w", err)
	}
	// The name filter matches a part of the name
	if slices.ContainsFunc(networks.Items, func(n network.Summary) bool { return n.Name == appsNetwork }) {
		return nil
	}
	slog.Info("creating the apps network", slog.String("name", appsNetwork))
	if _, err := docker.NetworkCreate(ctx, appsNetwork, dockerClient.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		return fmt.Errorf("failed to create the %s network: %w", appsNetwork, err)
	}
	return nil
}

// getContainerWithLabel returns the container carrying the label, running or not, nil without one.
func getContainerWithLabel(ctx context.Context, docker dockerClient.APIClient, label string) (*container.Summary, error) {
	containers, err := dockerhelper.Containers(ctx, docker, label+"=true")
	if err != nil {
		return nil, fmt.Errorf("failed to list the %s container: %w", label, err)
	}
	if len(containers) == 0 {
		return nil, nil
	}
	return &containers[0], nil
}

// stopContainerWithLabel stops the container carrying the label, which publishes ports even in a restart loop.
func stopContainerWithLabel(ctx context.Context, docker dockerClient.APIClient, label string) error {
	current, err := getContainerWithLabel(ctx, docker, label)
	if err != nil || current == nil || (current.State != container.StateRunning && current.State != container.StateRestarting) {
		return err
	}
	slog.Debug("stopping container", slog.String("label", label), slog.String("id", current.ID))
	if _, err := docker.ContainerStop(ctx, current.ID, dockerClient.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("failed to stop %s: %w", containerName(*current), err)
	}
	return nil
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
		return stopAppChild(ctx, cfg, appContainer{project: c.Labels[composeProjectLabel]})
	})
	return errors.Join(err, clearWantedApp(cfg, nil), stopSidecars(ctx, docker, func(StreamMessage) {}), removeSharedLauncher(ctx, docker))
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

// stopAppChild stops the app the container runs. An unreachable supervisor runs nothing.
func stopAppChild(ctx context.Context, cfg config.Configuration, c appContainer) error {
	resp, err := c.client(cfg).WithTimeout(launcherStopTimeout).Stop(ctx)
	var launcherErr *launcher.Error
	switch {
	case errors.As(err, &launcherErr):
		return fmt.Errorf("failed to stop the app: %w", err)
	case err != nil:
		slog.Debug("app container unreachable, nothing to stop", slog.String("project", c.project), slog.String("error", err.Error()))
	default:
		for _, s := range resp.Stopped {
			slog.Debug("app stopped", slog.String("project", c.project), slog.Int("pid", s.PID), slog.Any("exit_code", s.ExitCode), slog.Any("signal", s.Signal))
		}
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

// destroyAppContainer removes the container of the app with its runtime folder and its compose file.
func destroyAppContainer(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, appPath *paths.Path) error {
	c := appContainerOf(cfg, appPath)
	containers, err := getAppContainers(ctx, docker, c.project)
	if err != nil {
		return err
	}
	if main, ok := containers[mainServiceName]; ok {
		if err := removeContainers(ctx, docker, []container.Summary{main}); err != nil {
			return err
		}
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

// removeOrphanAppContainers removes the containers of the apps deleted outside the daemon.
func removeOrphanAppContainers(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration) error {
	containers, err := listAppMainContainers(ctx, docker)
	if err != nil {
		return err
	}
	orphans := orphanAppContainers(containers)
	for _, c := range orphans {
		slog.Info("removing the container of a deleted app", slog.String("container", containerName(c)))
	}
	return removeAppContainers(ctx, docker, cfg, orphans)
}

// PrewarmApps brings up the container of every app of the board, without starting them, one at a time.
func PrewarmApps(ctx context.Context, docker command.Cli, cfg config.Configuration, platform platform.Platform) {
	var running *paths.Path
	if wanted := readWantedApp(cfg); wanted != nil {
		running = wanted.App
	}
	for _, appPath := range warmApps(cfg) {
		if running != nil && running.EqualsTo(appPath) {
			continue
		}
		err := withAppsLock(ctx, cfg, func() error {
			elapsed, err := ensureAppContainer(ctx, docker, cfg, platform, appContainerOf(cfg, appPath), appPath, func(StreamMessage) {})
			slog.Debug("app container warmed up", slog.String("app", appPath.String()), slog.Duration("elapsed", elapsed))
			return err
		})
		if err != nil {
			slog.Debug("failed to warm up an app container", slog.String("app", appPath.String()), slog.String("error", err.Error()))
		}
	}
}

// EnsureRuntime removes the app containers of an older image or of a deleted app, pulls the ingress image and makes the
// ingress publish the ports of every app of the board, listening on none until an app starts.
func EnsureRuntime(
	ctx context.Context,
	docker command.Cli,
	cfg config.Configuration,
	bricksIndex *bricksindex.BricksIndex,
) (time.Duration, error) {
	started := time.Now()
	err := withAppsLock(ctx, cfg, func() error {
		if err := removeStaleAppContainers(ctx, docker.Client(), cfg); err != nil {
			slog.Warn("failed to remove the app containers of an older image", slog.String("error", err.Error()))
		}
		if err := removeOrphanAppContainers(ctx, docker.Client(), cfg); err != nil {
			slog.Warn("failed to remove the containers of deleted apps", slog.String("error", err.Error()))
		}
		if err := ensureAppsNetwork(ctx, docker.Client()); err != nil {
			return err
		}
		return syncIngress(ctx, docker, cfg, boardAppPorts(cfg, bricksIndex))
	})
	return time.Since(started), err
}

// composeUpFile creates, recreates or starts the containers of one of our compose files, as
// compose up does, and returns once they are started.
func composeUpFile(ctx context.Context, docker command.Cli, file *paths.Path, cb func(StreamMessage)) error {
	prj, err := loader.LoadWithContext(ctx,
		types.ConfigDetails{
			ConfigFiles: []types.ConfigFile{{Filename: file.String()}},
			WorkingDir:  file.Parent().String(),
			Environment: types.Mapping{},
		},
		func(o *loader.Options) { o.ResolvePaths = true },
	)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", file, err)
	}
	prj.ComposeFiles = []string{file.String()}
	return dockerhelper.ComposeUp(ctx, docker, prj, func(line string) { cb(StreamMessage{data: line}) })
}
