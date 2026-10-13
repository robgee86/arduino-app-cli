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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/servicesindex"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

const testPythonImage = "app-bricks:python-apps-base:dev-latest"

// setTestBricks writes a bricks index with a UI brick, a brick with a sidecar publishing 1337, and a plain brick.
func setTestBricks(t *testing.T, cfg config.Configuration) (*bricksindex.BricksIndex, *servicesindex.ServicesIndex) {
	t.Helper()
	require.NoError(t, cfg.AssetDir().Join("bricks-list.yaml").WriteFile([]byte(`
bricks:
- id: arduino:web_ui
  name: Web UI
  ports: ["7000"]
- id: arduino:custom_ui
  name: Custom UI
  ports: ["8000"]
- id: arduino:object_detection
  name: Object Detection
  variables:
  - name: BOARD_NAME
    default_value: from-the-brick
  - name: EI_MODEL
    default_value: /models/yolo.eim
- id: arduino:plain
  name: Plain
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

func TestSplitMainService(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, servicesIndex := setTestBricks(t, cfg)
	appEnv := types.Mapping{"FOO": "bar"}

	t.Run("sidecars only, on the shared network", func(t *testing.T) {
		a := newTestApp(t, cfg, "with-sidecar", "arduino:web_ui", "arduino:object_detection")
		project := renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, appEnv)
		envs := splitMainService(project)

		assert.Equal(t, "with-sidecar", project.Name)
		assert.Equal(t, []string{"ei-obj-detection-runner"}, project.ServiceNames())
		sidecar := project.Services["ei-obj-detection-runner"]
		assert.Empty(t, sidecar.DependsOn)
		assert.Equal(t, "true", sidecar.Labels[DockerAppLabel])
		assert.Equal(t, a.FullPath.String(), sidecar.Labels[DockerAppPathLabel])
		assert.Equal(t, "bar", *sidecar.Environment["FOO"])

		network := project.Networks["default"]
		assert.Equal(t, launcherNetwork, network.Name)
		assert.True(t, bool(network.External))

		assert.Equal(t, "bar", envs["FOO"])
		assert.Equal(t, a.FullPath.String(), envs["APP_HOME"])
	})

	t.Run("no sidecars, no services", func(t *testing.T) {
		a := newTestApp(t, cfg, "ui-only", "arduino:web_ui")
		project := renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, appEnv)
		splitMainService(project)
		assert.Empty(t, project.Services)
	})

	t.Run("the render keeps the main service for the classic mode", func(t *testing.T) {
		a := newTestApp(t, cfg, "classic", "arduino:web_ui", "arduino:object_detection")
		project := renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, appEnv)

		assert.ElementsMatch(t, []string{"main", "ei-obj-detection-runner"}, project.ServiceNames())
		main := project.Services["main"]
		assert.Equal(t, types.ShellCommand{"/run.sh"}, main.Entrypoint)
		assert.Equal(t, "service_healthy", main.DependsOn["ei-obj-detection-runner"].Condition)
		assert.Equal(t, "true", main.Labels[DockerAppMainLabel])
		assert.Contains(t, main.Volumes, types.ServiceVolumeConfig{Type: "bind", Source: a.FullPath.String(), Target: "/app"})
		assert.False(t, bool(project.Networks["default"].External))
	})
}

func TestGenerateLauncherComposeFile(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)
	require.NoError(t, cfg.AppsDir().MkdirAll())
	require.NoError(t, cfg.ExamplesBaseDir().MkdirAll())
	require.NoError(t, cfg.RuntimeDir().MkdirAll())
	boardEnvs := types.Mapping{"BOARD_NAME": "unoq", "HOST_IP": "10.0.0.2"}

	changed, err := generateLauncherComposeFile(bricksIndex, testPythonImage, cfg, boardEnvs, platform.Platform{})
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

	// The brick ports, not the sidecar ones
	published := make([]uint32, 0, len(svc.Ports))
	for _, p := range svc.Ports {
		published = append(published, p.Target)
	}
	assert.Equal(t, []uint32{7000, 8000}, published)

	assert.Equal(t, types.MappingWithEquals{
		"BOARD_NAME":          new("unoq"),
		"HOST_IP":             new("10.0.0.2"),
		"APP_LAUNCHER_PORTS":  new("7000,8000"),
		"APP_LAUNCHER_SOCKET": new("/run/arduino-app-cli/app-launcher.sock"),
	}, svc.Environment)

	targets := make(map[string]string, len(svc.Volumes))
	for _, v := range svc.Volumes {
		targets[v.Target] = v.Source
	}
	assert.Equal(t, cfg.AppsDir().String(), targets[cfg.AppsDir().String()])
	assert.Equal(t, cfg.ExamplesBaseDir().String(), targets[cfg.ExamplesBaseDir().String()])
	assert.Equal(t, cfg.RuntimeDir().String(), targets[launcherSocketDir])
	assert.NotContains(t, targets, "/app")

	network := project.Networks["default"]
	assert.Equal(t, launcherNetwork, network.Name)
	assert.False(t, bool(network.External))

	t.Run("a second generation changes nothing", func(t *testing.T) {
		changed, err := generateLauncherComposeFile(bricksIndex, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("another board environment changes it", func(t *testing.T) {
		changed, err := generateLauncherComposeFile(bricksIndex, testPythonImage, cfg, types.Mapping{"HOST_IP": "10.0.0.3"}, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
	})
}

func TestUncoveredAppPort(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)

	assert.Equal(t, []int{7000, 8000}, launcherPorts(bricksIndex))

	_, uncovered := uncoveredAppPort(app.ArduinoApp{Descriptor: app.AppDescriptor{Ports: []int{7000, 8000}}}, bricksIndex)
	assert.False(t, uncovered)

	port, uncovered := uncoveredAppPort(app.ArduinoApp{Descriptor: app.AppDescriptor{Ports: []int{7000, 8080}}}, bricksIndex)
	assert.True(t, uncovered)
	assert.Equal(t, 8080, port)
}

func TestIsInsideLauncherRoots(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	assert.True(t, isInsideLauncherRoots(cfg.AppsDir().Join("my-app"), cfg))
	assert.True(t, isInsideLauncherRoots(cfg.ExamplesBaseDir().Join("inspirational", "common", "blink"), cfg))
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
