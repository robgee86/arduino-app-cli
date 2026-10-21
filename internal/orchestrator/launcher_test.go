// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
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

// writeTestApp writes an app the launcher can run in dir.
func writeTestApp(t *testing.T, dir *paths.Path, descriptor string) {
	t.Helper()
	require.NoError(t, dir.Join("python").MkdirAll())
	require.NoError(t, dir.Join("python", "main.py").WriteFile([]byte("print('hi')\n")))
	require.NoError(t, dir.Join("app.yaml").WriteFile([]byte(descriptor)))
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

func TestSplitMainService(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, servicesIndex := setTestBricks(t, cfg)

	a := newTestApp(t, cfg, "with-sidecar", "arduino:web_ui", "arduino:object_detection")
	project := renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"})
	envs := splitMainService(project)

	assert.Equal(t, []string{"ei-obj-detection-runner"}, project.ServiceNames())
	assert.Equal(t, "bar", envs["FOO"])
	assert.Equal(t, a.FullPath.String(), envs["APP_HOME"])
}

func TestNewAppProject(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, servicesIndex := setTestBricks(t, cfg)
	a := newTestApp(t, cfg, "with-sidecar", "arduino:web_ui", "arduino:object_detection")

	project, err := newAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"}))
	require.NoError(t, err)
	assert.Equal(t, "with-sidecar", project.name)
	assert.Equal(t, []string{"ei-obj-detection-runner"}, project.sidecars(), "the main service is the launcher's")
	assert.Equal(t, types.NetworkConfig{Name: launcherNetwork, External: true}, project.prj.Networks["default"], "the sidecars join the launcher network")
	assert.Equal(t, "bar", project.appEnvs["FOO"])

	t.Run("the same app renders the same fingerprint", func(t *testing.T) {
		again, err := newAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"}))
		require.NoError(t, err)
		assert.Equal(t, project.fingerprint, again.fingerprint)
	})

	t.Run("another app environment changes the sidecars, as on main", func(t *testing.T) {
		again, err := newAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "baz"}))
		require.NoError(t, err)
		assert.NotEqual(t, project.fingerprint, again.fingerprint)
	})

	t.Run("the record remembers the fingerprint", func(t *testing.T) {
		assert.Empty(t, project.recordedFingerprint(cfg))
		require.NoError(t, project.recordFingerprint(cfg))
		assert.Equal(t, project.fingerprint, project.recordedFingerprint(cfg))
		assert.True(t, cfg.DataDir().Join("apps", "with-sidecar", "project.json").Exist())
	})
}

// inspectingDocker answers the container list of one project and an inspect per container id, the
// health of a container improving at each inspect.
type inspectingDocker struct {
	*fakeDocker
	containers []container.Summary
	health     map[string][]container.HealthStatus
	mu         sync.Mutex
}

func (f *inspectingDocker) ContainerList(context.Context, dockerClient.ContainerListOptions) (dockerClient.ContainerListResult, error) {
	return dockerClient.ContainerListResult{Items: f.containers}, nil
}

func (f *inspectingDocker) ContainerInspect(_ context.Context, id string, _ dockerClient.ContainerInspectOptions) (dockerClient.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := &container.State{Running: true, Status: container.StateRunning}
	if steps := f.health[id]; len(steps) > 0 {
		state.Health = &container.Health{Status: steps[0]}
		if len(steps) > 1 {
			f.health[id] = steps[1:]
		}
	}
	return dockerClient.ContainerInspectResult{Container: container.InspectResponse{State: state}}, nil
}

func TestWaitSidecarsReady(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, servicesIndex := setTestBricks(t, cfg)
	a := newTestApp(t, cfg, "with-sidecar", "arduino:web_ui", "arduino:object_detection")
	project, err := newAppProject(renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{}))
	require.NoError(t, err)
	const runner = "ei-obj-detection-runner"
	require.NotNil(t, project.prj.Services[runner].HealthCheck, "the test sidecar declares a healthcheck")
	ctr := func(service, id string) container.Summary {
		return container.Summary{ID: id, Names: []string{"/" + project.name + "-" + service + "-1"}, State: container.StateRunning, Labels: map[string]string{composeProjectLabel: project.name, composeServiceLabel: service}}
	}

	t.Run("a healthy sidecar is waited for until its healthcheck passes", func(t *testing.T) {
		docker := &inspectingDocker{fakeDocker: newFakeDocker(t), containers: []container.Summary{ctr(runner, "runner")},
			health: map[string][]container.HealthStatus{"runner": {container.Starting, container.Starting, container.Healthy}}}
		require.NoError(t, waitSidecarsReady(t.Context(), docker, project))
		assert.Equal(t, []container.HealthStatus{container.Healthy}, docker.health["runner"], "polled until healthy")
	})

	t.Run("an unhealthy sidecar fails the start", func(t *testing.T) {
		docker := &inspectingDocker{fakeDocker: newFakeDocker(t), containers: []container.Summary{ctr(runner, "runner")},
			health: map[string][]container.HealthStatus{"runner": {container.Unhealthy}}}
		require.ErrorContains(t, waitSidecarsReady(t.Context(), docker, project), "ei-obj-detection-runner-1 is not ready: the container is unhealthy")
	})

	t.Run("a missing sidecar container fails the start", func(t *testing.T) {
		docker := &inspectingDocker{fakeDocker: newFakeDocker(t)}
		require.ErrorContains(t, waitSidecarsReady(t.Context(), docker, project), "has no container")
	})

	t.Run("an app without sidecars waits for nothing", func(t *testing.T) {
		plain := newTestApp(t, cfg, "plain", "arduino:web_ui")
		project, err := newAppProject(renderTestApp(t, cfg, &plain, bricksIndex, servicesIndex, types.Mapping{}))
		require.NoError(t, err)
		require.NoError(t, waitSidecarsReady(t.Context(), newFakeDocker(t), project))
	})
}

func TestGenerateLauncherComposeFile(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	require.NoError(t, cfg.AppsDir().MkdirAll())
	require.NoError(t, cfg.ExamplesBaseDir().MkdirAll())
	require.NoError(t, cfg.ReleasesDir().MkdirAll())
	require.NoError(t, cfg.RuntimeDir().MkdirAll())
	boardEnvs := types.Mapping{"BOARD_NAME": "unoq", "HOST_IP": "10.0.0.2"}

	changed, err := generateLauncherComposeFile([]int{8000, 7100, 8000}, testPythonImage, cfg, boardEnvs, platform.Platform{})
	require.NoError(t, err)
	assert.True(t, changed)

	project := loadCompose(t, launcherComposeFile(cfg))
	assert.Equal(t, launcherProject, project.Name)
	assert.Equal(t, []string{launcherServiceName}, project.ServiceNames())
	svc := project.Services[launcherServiceName]

	assert.Equal(t, testPythonImage, svc.Image)
	assert.Equal(t, types.ShellCommand{"/run.sh", "serve"}, svc.Entrypoint)
	assert.True(t, *svc.Init)
	assert.Equal(t, "unless-stopped", svc.Restart)
	assert.Equal(t, types.Duration(10*time.Second), *svc.StopGracePeriod)
	assert.Equal(t, map[string]string{DockerLauncherLabel: "true"}, map[string]string(svc.Labels))

	require.NotNil(t, svc.HealthCheck)
	assert.Equal(t, types.HealthCheckTest{"CMD", "arduino-app-launcher", "ping"}, svc.HealthCheck.Test)
	assert.Equal(t, types.Duration(2*time.Second), *svc.HealthCheck.Interval)
	assert.Equal(t, types.Duration(60*time.Second), *svc.HealthCheck.StartPeriod)
	assert.Equal(t, types.Duration(500*time.Millisecond), *svc.HealthCheck.StartInterval)

	t.Run("publishes the app ports, sorted once each", func(t *testing.T) {
		published := make([]string, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			published = append(published, fmt.Sprintf("%s:%d", p.Published, p.Target))
		}
		assert.Equal(t, []string{"7100:7100", "8000:8000"}, published)
		assert.Empty(t, svc.Ports[0].HostIP, "on every address, IPv6 included")
	})

	assert.Equal(t, types.MappingWithEquals{
		"BOARD_NAME":          new("unoq"),
		"HOST_IP":             new("10.0.0.2"),
		"APP_LAUNCHER_SOCKET": new("/run/arduino-app-cli/app-launcher.sock"),
		// The CLI waits for a start as long as the launcher may prepare it
		"APP_LAUNCHER_PREPARE_TIMEOUT_S": new("600"),
	}, svc.Environment)

	targets := make(map[string]string, len(svc.Volumes))
	for _, v := range svc.Volumes {
		targets[v.Target] = v.Source
	}
	assert.Equal(t, cfg.AppsDir().String(), targets[cfg.AppsDir().String()])
	assert.Equal(t, cfg.ExamplesBaseDir().String(), targets[cfg.ExamplesBaseDir().String()])
	assert.Equal(t, cfg.ReleasesDir().String(), targets[cfg.ReleasesDir().String()], "the installed releases run in place too")
	assert.Equal(t, cfg.RuntimeDir().String(), targets[launcherSocketDir])
	assert.NotContains(t, targets, "/app")

	network := project.Networks["default"]
	assert.Equal(t, "arduino-app-launcher_default", network.Name, "a network of its own, the launcher joins the network of the app it runs")
	assert.False(t, bool(network.External))

	t.Run("a second generation changes nothing", func(t *testing.T) {
		changed, err := generateLauncherComposeFile([]int{7100, 8000}, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("the same ports in another order change nothing", func(t *testing.T) {
		changed, err := generateLauncherComposeFile([]int{8000, 7100}, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("another board environment changes it", func(t *testing.T) {
		changed, err := generateLauncherComposeFile([]int{7100, 8000}, testPythonImage, cfg, types.Mapping{"HOST_IP": "10.0.0.3"}, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("a new app port changes it", func(t *testing.T) {
		changed, err := generateLauncherComposeFile([]int{7100, 8000, 9000}, testPythonImage, cfg, types.Mapping{"HOST_IP": "10.0.0.3"}, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
		svc := loadCompose(t, launcherComposeFile(cfg)).Services[launcherServiceName]
		require.Len(t, svc.Ports, 3)
		assert.Equal(t, uint32(9000), svc.Ports[2].Target)
	})

	t.Run("no ports, nothing published", func(t *testing.T) {
		changed, err := generateLauncherComposeFile(nil, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
		data, err := launcherComposeFile(cfg).ReadFile()
		require.NoError(t, err)
		assert.NotContains(t, string(data), "ports:")
		assert.Empty(t, loadCompose(t, launcherComposeFile(cfg)).Services[launcherServiceName].Ports)
	})

	t.Run("a preload override reaches the launcher and changes the file", func(t *testing.T) {
		cfg.LauncherPreload = "arduino.app_utils,arduino.app_peripherals.camera"
		changed, err := generateLauncherComposeFile(nil, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
		svc := loadCompose(t, launcherComposeFile(cfg)).Services[launcherServiceName]
		assert.Equal(t, new("arduino.app_utils,arduino.app_peripherals.camera"), svc.Environment["APP_LAUNCHER_PRELOAD"])
	})
}

func TestLauncherPorts(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)

	t.Run("no app, no port", func(t *testing.T) {
		assert.Empty(t, launcherPorts(cfg, bricksIndex))
	})

	writeTestApp(t, cfg.AppsDir().Join("with-web-ui"), "name: With Web UI\nbricks:\n- arduino:web_ui: {}\n")
	writeTestApp(t, cfg.ExamplesBaseDir().Join("inspirational", "common", "custom-ui"), "name: Custom UI\nbricks:\n- arduino:custom_ui: {}\n")
	writeTestApp(t, cfg.ReleasesDir().Join("own-port"), "name: Own Port\nports:\n- 9000\nbricks:\n- arduino:plain: {}\n")
	writeTestApp(t, cfg.AppsDir().Join("broken"), "name: [\n")
	writeTestApp(t, cfg.AppsDir().Join("no-tcp-port"), "name: No TCP Port\nports:\n- 70000\n- 7000\n")

	// The union of the apps in every root, sorted once each, without 9100 of the brick no app uses
	assert.Equal(t, []int{7000, 7100, 8000, 9000}, launcherPorts(cfg, bricksIndex))
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

func TestComposeUpReason(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		forceUp, changed, running bool
		wantUp                    bool
	}{
		{name: "boot or up, to follow the image", forceUp: true, running: true, wantUp: true},
		{name: "compose file changed", changed: true, running: true, wantUp: true},
		{name: "container not running", wantUp: true},
		{name: "start with nothing changed", running: true, wantUp: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, up := composeUpReason(tc.forceUp, tc.changed, tc.running)
			assert.Equal(t, tc.wantUp, up)
			assert.Equal(t, tc.wantUp, reason != "")
		})
	}
}

func TestPrepareLauncherRejectsWhatItCannotRun(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)
	noop := func(StreamMessage) {}

	elsewhere := app.ArduinoApp{Name: "elsewhere", FullPath: paths.New(t.TempDir())}
	err := prepareLauncher(t.Context(), nil, bricksIndex, elsewhere, nil, cfg, platform.Platform{}, noop)
	require.Error(t, err)
	assert.Contains(t, err.Error(), cfg.AppsDir().String())

	// Not a TCP port
	high := newTestApp(t, cfg, "high")
	err = prepareLauncher(t.Context(), nil, bricksIndex, high, []int{8000, 70000}, cfg, platform.Platform{}, noop)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot publish port 70000")
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

func TestIsInsideLauncherRoots(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	assert.True(t, isInsideLauncherRoots(cfg.AppsDir().Join("my-app"), cfg))
	assert.True(t, isInsideLauncherRoots(cfg.ExamplesBaseDir().Join("inspirational", "common", "blink"), cfg))
	assert.True(t, isInsideLauncherRoots(cfg.ReleasesDir().Join("my-release"), cfg))
	assert.False(t, isInsideLauncherRoots(paths.New("/somewhere/else/my-app"), cfg))
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

func TestLauncherWantedApp(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	first, second := cfg.AppsDir().Join("first"), cfg.AppsDir().Join("second")

	assert.Nil(t, readLauncherWantedApp(cfg))
	require.NoError(t, setLauncherWantedApp(cfg, first))
	assert.Equal(t, first.String(), readLauncherWantedApp(cfg).String())

	require.NoError(t, clearLauncherWantedApp(cfg, second))
	assert.Equal(t, first.String(), readLauncherWantedApp(cfg).String(), "another app keeps it")

	require.NoError(t, clearLauncherWantedApp(cfg, first))
	assert.Nil(t, readLauncherWantedApp(cfg))

	require.NoError(t, setLauncherWantedApp(cfg, second))
	require.NoError(t, clearLauncherWantedApp(cfg, nil))
	assert.Nil(t, readLauncherWantedApp(cfg))
}
