// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"os"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/servicesindex"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

const testPythonImage = "app-bricks:python-apps-base:dev-latest"

// setTestBricks writes a bricks index with three UI bricks, a brick with a sidecar publishing 1337, a
// brick whose sidecar mounts the app folder, and a plain brick.
func setTestBricks(t *testing.T, cfg config.Configuration) (*bricksindex.BricksIndex, *servicesindex.ServicesIndex) {
	t.Helper()
	require.NoError(t, cfg.AssetDir().Join("bricks-list.yaml").WriteFile([]byte(`
bricks:
- id: arduino:web_ui
  name: Web UI
  ports: ["7100"]
- id: arduino:custom_ui
  name: Custom UI
  ports: ["8000", "7100"]
- id: arduino:unused_ui
  name: Unused UI
  ports: ["9100"]
- id: arduino:storage
  name: Storage
- id: arduino:object_detection
  name: Object Detection
  variables:
  - name: BOARD_NAME
    default_value: from-the-brick
  - name: EI_MODEL
    default_value: /models/yolo.eim
- id: arduino:plain
  name: Plain
- id: arduino:cache
  name: Cache
`)))
	cacheDir := cfg.AssetDir().Join("compose", "arduino", "cache")
	require.NoError(t, cacheDir.MkdirAll())
	require.NoError(t, cacheDir.Join("brick_compose.yaml").WriteFile([]byte(`
services:
  cache-db:
    image: redis:7-alpine
    volumes:
      - cache-data:/data
volumes:
  cache-data: {}
`)))
	composeDir := cfg.AssetDir().Join("compose", "arduino", "object_detection")
	require.NoError(t, composeDir.MkdirAll())
	require.NoError(t, composeDir.Join("brick_compose.yaml").WriteFile([]byte(`
services:
  ei-obj-detection-runner:
    image: arduino/ei-models-runner:latest
    ports:
      - 127.0.0.1:1337:1337
    healthcheck:
      test: ["CMD", "true"]
`)))
	storageDir := cfg.AssetDir().Join("compose", "arduino", "storage")
	require.NoError(t, storageDir.MkdirAll())
	require.NoError(t, storageDir.Join("brick_compose.yaml").WriteFile([]byte(`
services:
  storage-db:
    image: influxdb:2.9-alpine
    volumes:
      - "${APP_HOME:-.}/data/db:/var/lib/db"
`)))
	require.NoError(t, cfg.AssetDir().Join("services").MkdirAll())
	servicesIndex, err := servicesindex.Load(platform.GetPlatform(nil), cfg.AssetDir().Join("services"))
	require.NoError(t, err)
	bricksIndex, err := bricksindex.Load(platform.GetPlatform(nil), cfg.AssetDir())
	require.NoError(t, err)
	return bricksIndex, servicesIndex
}

func newTestApp(t *testing.T, cfg config.Configuration, name string, bricks ...string) app.ArduinoApp {
	t.Helper()
	a := app.ArduinoApp{Name: name, FullPath: cfg.AppsDir().Join(name)}
	for _, id := range bricks {
		a.Descriptor.Bricks = append(a.Descriptor.Bricks, app.Brick{ID: id})
	}
	require.NoError(t, a.ProvisioningStateDir().MkdirAll())
	return a
}

// loadCompose parses compose files the way docker compose does, includes and overrides resolved.
func loadCompose(t *testing.T, files ...*paths.Path) *types.Project {
	t.Helper()
	configFiles := make([]types.ConfigFile, 0, len(files))
	for _, file := range files {
		configFiles = append(configFiles, types.ConfigFile{Filename: file.String()})
	}
	project, err := loader.LoadWithContext(t.Context(), types.ConfigDetails{
		ConfigFiles: configFiles,
		WorkingDir:  files[0].Parent().String(),
		Environment: types.NewMapping(os.Environ()),
	})
	require.NoError(t, err)
	return project
}

// renderTestApp resolves and renders the app compose project as a start does.
func renderTestApp(t *testing.T, cfg config.Configuration, a *app.ArduinoApp, bricksIndex *bricksindex.BricksIndex, servicesIndex *servicesindex.ServicesIndex, appEnv types.Mapping) *types.Project {
	t.Helper()
	p := &Provision{pythonImage: testPythonImage, appsDir: cfg.AppsDir()}
	require.NoError(t, p.Resolve(a, a.ProvisioningStateDir(), bricksIndex, servicesIndex, cfg, appEnv, platform.Platform{}))
	prj, err := p.Render(t.Context(), a, hostEnvironment(t.Context(), a.FullPath, cfg).Merge(appEnv), types.Mapping{})
	require.NoError(t, err)
	return prj
}

func TestAdaptAppProject(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, servicesIndex := setTestBricks(t, cfg)
	a := newTestApp(t, cfg, "with-sidecar", "arduino:web_ui", "arduino:object_detection")
	c := appContainerOf(cfg, a.FullPath)
	boardEnvs := types.Mapping{"BOARD_NAME": "unoq", "HOST_IP": "10.0.0.2"}

	project, err := adaptAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"}), c, cfg, boardEnvs)
	require.NoError(t, err)

	assert.Equal(t, []string{"ei-obj-detection-runner", mainServiceName}, project.services(), "the sidecars stay in the project")
	assert.Equal(t, "bar", project.appEnvs["FOO"], "the app environment goes with the start")
	assert.Equal(t, a.FullPath.String(), project.appEnvs["APP_HOME"])
	assert.Equal(t, []int{7100}, project.ports())

	main := project.prj.Services[mainServiceName]
	assert.Equal(t, types.ShellCommand{"/run.sh", "serve"}, main.Entrypoint)
	assert.Equal(t, "/app", main.WorkingDir)
	assert.Equal(t, types.RestartPolicyNo, main.Restart)
	assert.Equal(t, types.Duration(10*time.Second), *main.StopGracePeriod)
	assert.Nil(t, main.HealthCheck, "a healthcheck would run a python in every warm container")
	assert.Empty(t, main.DependsOn, "the bricks wait for the sidecars, not compose")
	assert.True(t, *main.Init)
	assert.Equal(t, types.MappingWithEquals{
		"BOARD_NAME": new("unoq"),
		"HOST_IP":    new("10.0.0.2"),
		// The CLI waits for a start as long as the supervisor may prepare it
		"APP_LAUNCHER_PREPARE_TIMEOUT_S": new("600"),
	}, main.Environment, "nothing of the app: its environment goes with each start")
	targets := make(map[string]string, len(main.Volumes))
	for _, v := range main.Volumes {
		targets[v.Target] = v.Source
	}
	assert.Equal(t, a.FullPath.String(), targets["/app"])
	assert.Equal(t, c.runtimeDir(cfg).String(), targets["/run/arduino-app-cli"])
	assert.Equal(t, "/dev", targets["/dev"])
	assert.Equal(t, "true", main.Labels[DockerAppMainLabel])

	t.Run("the same app renders the same fingerprint", func(t *testing.T) {
		again, err := adaptAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"}), c, cfg, boardEnvs)
		require.NoError(t, err)
		assert.Equal(t, project.fingerprint, again.fingerprint)
	})

	t.Run("another app environment changes nothing of the main service", func(t *testing.T) {
		again, err := adaptAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "baz"}), c, cfg, boardEnvs)
		require.NoError(t, err)
		assert.Equal(t, project.prj.Services[mainServiceName], again.prj.Services[mainServiceName])
		assert.Equal(t, "baz", again.appEnvs["FOO"])
	})

	t.Run("a new port changes the fingerprint", func(t *testing.T) {
		b := a
		b.Descriptor.Ports = []int{9000}
		again, err := adaptAppProject(renderTestApp(t, cfg, &b, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"}), c, cfg, boardEnvs)
		require.NoError(t, err)
		assert.Equal(t, []int{7100, 9000}, again.ports())
		assert.NotEqual(t, project.fingerprint, again.fingerprint)
	})

	t.Run("the record remembers the fingerprint", func(t *testing.T) {
		assert.Empty(t, c.recordedFingerprint(cfg))
		require.NoError(t, c.recordFingerprint(cfg, project.fingerprint))
		assert.Equal(t, project.fingerprint, c.recordedFingerprint(cfg))
	})
}

func TestAppContainerNames(t *testing.T) {
	short := appContainer{project: "my-app"}
	assert.Equal(t, "my-app-main-1", short.containerName(), "the compose name of the container")
	assert.Equal(t, "my-app", short.key())

	// An example of the board, named after its path from the apps folder
	long := appContainer{project: composeProjectName(
		paths.New("/var/lib/arduino-app-cli/examples/inspirational/common/a-rather-long-example-name"),
		paths.New("/home/arduino/ArduinoApps"),
	)}
	assert.NotEqual(t, long.key(), appContainer{project: long.project + "x"}.key(), "unique per project")
	socket := "/run/arduino-app-cli/apps/" + long.key() + "/app-launcher.sock"
	assert.Less(t, len(socket), 108, "a Unix socket path")
}

func TestPortHolders(t *testing.T) {
	running := func(name string, ports ...uint16) container.Summary {
		c := container.Summary{ID: name, Names: []string{"/" + name}, State: container.StateRunning}
		for _, p := range ports {
			c.Ports = append(c.Ports, container.PortSummary{PrivatePort: p, PublicPort: p, Type: "tcp"})
		}
		return c
	}
	exited := running("exited-main-1")
	exited.State = container.StateExited
	docker := newFakeDocker(t)
	docker.byLabel = map[string][]container.Summary{DockerAppMainLabel: {running("web-main-1", 7000), running("other-main-1", 7000, 8080), running("plain-main-1"), exited, running("mine-main-1", 7000)}}
	mine := appContainer{project: "mine"}

	holders, err := portHolders(t.Context(), docker, mine, []int{7000})
	require.NoError(t, err)
	assert.Equal(t, []string{"web-main-1", "other-main-1"}, names(holders), "the running containers of other apps on a port of the app")

	holders, err = portHolders(t.Context(), docker, mine, []int{9000})
	require.NoError(t, err)
	assert.Empty(t, holders)

	holders, err = portHolders(t.Context(), docker, mine, nil)
	require.NoError(t, err)
	assert.Empty(t, holders, "an app without ports asks Docker nothing")
}

func names(containers []container.Summary) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		names = append(names, containerName(c))
	}
	return names
}

func TestStopAppProcessesKeepsTheContainerWarm(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	require.NoError(t, cfg.RuntimeDir().MkdirAll())
	// The app container is unreachable: the stop falls back to a restart, which finds no container
	stopped := newTestApp(t, cfg, "stopped")
	stopped.MainPythonFile = stopped.FullPath.Join("python", "main.py")
	startedAt := time.Now()

	t.Run("another app keeps the sidecars zone", func(t *testing.T) {
		docker := newFakeDocker(t)
		require.NoError(t, setWantedApp(cfg, cfg.AppsDir().Join("other"), startedAt))
		require.NoError(t, stopAppProcesses(t.Context(), docker, stopped, cfg, nil))
		assert.Equal(t, []string{"restart stopped-main-1"}, docker.calls)
		assert.NotNil(t, readWantedApp(cfg))
	})

	t.Run("the running app is forgotten, its container is not removed", func(t *testing.T) {
		docker := newFakeDocker(t)
		require.NoError(t, setWantedApp(cfg, stopped.FullPath, startedAt))
		require.NoError(t, stopAppProcesses(t.Context(), docker, stopped, cfg, nil))
		assert.Equal(t, []string{"restart stopped-main-1"}, docker.calls)
		assert.Nil(t, readWantedApp(cfg))
	})
}

func TestWarmApps(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	a, b := newTestApp(t, cfg, "a"), newTestApp(t, cfg, "b")
	for _, p := range []*paths.Path{a.FullPath, b.FullPath} {
		require.NoError(t, p.Join("app.yaml").WriteFile([]byte("name: app\n")))
	}
	require.NoError(t, cfg.AppsDir().Join("not-an-app").MkdirAll())
	warm := warmApps(cfg)
	assert.ElementsMatch(t, []string{a.FullPath.String(), b.FullPath.String()}, warm.AsStrings(), "every user app, nothing else")
}

func TestOrphanAppContainers(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	kept := newTestApp(t, cfg, "kept")
	appContainer := func(id, appPath string) container.Summary {
		return container.Summary{ID: id, Labels: map[string]string{DockerAppMainLabel: "true", DockerAppPathLabel: appPath}}
	}
	containers := []container.Summary{
		appContainer("kept", kept.FullPath.String()),
		appContainer("gone", cfg.AppsDir().Join("gone").String()),
		{ID: "unlabelled", Labels: map[string]string{DockerAppMainLabel: "true"}},
	}
	orphans := orphanAppContainers(containers)
	require.Len(t, orphans, 1)
	assert.Equal(t, "gone", orphans[0].ID)
}

func TestLauncherAppPorts(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)

	t.Run("brick defaults, then app.yaml", func(t *testing.T) {
		a := app.ArduinoApp{Descriptor: app.AppDescriptor{Ports: []int{9000}, Bricks: []app.Brick{{ID: "arduino:custom_ui"}}}}
		assert.Equal(t, []int{7100, 8000, 9000}, appPorts(a, bricksIndex))
	})

	t.Run("a local brick declares its ports", func(t *testing.T) {
		a := app.ArduinoApp{
			Descriptor:  app.AppDescriptor{Bricks: []app.Brick{{ID: "local:ui"}}},
			LocalBricks: []bricksindex.Brick{{ID: "local:ui", Ports: []string{"7200"}}},
		}
		assert.Equal(t, []int{7200}, appPorts(a, bricksIndex))
		assert.Empty(t, bricksIndex.AppBricks, "the shared index is left as is")
	})

	t.Run("no bricks with ports, no port", func(t *testing.T) {
		a := app.ArduinoApp{Descriptor: app.AppDescriptor{Bricks: []app.Brick{{ID: "arduino:plain"}}}}
		assert.Empty(t, appPorts(a, bricksIndex))
	})
}

func TestCheckAppPorts(t *testing.T) {
	require.NoError(t, checkAppPorts([]int{1, 7000, 65535}))
	require.ErrorContains(t, checkAppPorts([]int{8000, 70000}), "cannot publish port 70000")
	require.ErrorContains(t, checkAppPorts([]int{0}), "cannot publish port 0")
}

func TestLauncherStartError(t *testing.T) {
	installOutput := "failed to install the app requirements\nERROR: No matching distribution found for nump"
	err := launcherStartError(&launcher.Error{Code: launcher.CodePrepareFailed, Message: installOutput})
	assert.EqualError(t, err, installOutput, "shown as is")
	var prepareErr *PrepareError
	assert.ErrorAs(t, err, &prepareErr)

	busy := &launcher.Error{Code: launcher.CodeBusy, Message: "a start is in progress"}
	assert.Same(t, busy, launcherStartError(busy))
	assert.NoError(t, launcherStartError(nil))
}

func TestBoardEnvironment(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	board := boardEnvironment(t.Context(), platform.Platform{BoardName: "unoq"}, cfg)
	assert.Equal(t, "unoq", board["BOARD_NAME"])
	assert.Equal(t, cfg.ModelsDir().String(), board["MODELS_PATH"])
	assert.Equal(t, "/run/user/1000", board["XDG_RUNTIME_DIR"])
	assert.NotContains(t, board, "APP_HOME")
	assert.NotContains(t, board, "VIDEO_DEVICE")
}

func TestWantedApp(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	first, second := cfg.AppsDir().Join("first"), cfg.AppsDir().Join("second")
	startedAt := time.Date(2026, 10, 20, 10, 0, 0, 123456789, time.UTC)

	assert.Nil(t, readWantedApp(cfg))
	require.NoError(t, setWantedApp(cfg, first, startedAt))
	wanted := readWantedApp(cfg)
	require.NotNil(t, wanted)
	assert.Equal(t, first.String(), wanted.App.String())
	assert.True(t, startedAt.Equal(wanted.StartedAt), "the start time keeps its nanoseconds")

	require.NoError(t, clearWantedApp(cfg, second))
	assert.Equal(t, first.String(), readWantedApp(cfg).App.String(), "another app keeps it")

	require.NoError(t, clearWantedApp(cfg, first))
	assert.Nil(t, readWantedApp(cfg))

	require.NoError(t, setWantedApp(cfg, second, startedAt))
	require.NoError(t, clearWantedApp(cfg, nil))
	assert.Nil(t, readWantedApp(cfg))

	require.NoError(t, wantedAppFile(cfg).WriteFile([]byte(first.String())))
	assert.Nil(t, readWantedApp(cfg), "a record of an older version is none")
}
