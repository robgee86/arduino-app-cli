// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"maps"
	"os"
	"slices"
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

func TestGenerateSidecarsZone(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, servicesIndex := setTestBricks(t, cfg)
	zoneFor := func(a app.ArduinoApp) sidecarsZone {
		prj := renderTestApp(t, cfg, &a, bricksIndex, servicesIndex, types.Mapping{"FOO": "bar"})
		splitMainService(prj)
		zone, err := generateSidecarsZone(prj, cfg)
		require.NoError(t, err)
		return zone
	}
	zoneFile := func() string {
		compose, err := sidecarsComposeFile(cfg).ReadFile()
		require.NoError(t, err)
		return string(compose)
	}

	first := newTestApp(t, cfg, "first", "arduino:web_ui", "arduino:object_detection")
	zone := zoneFor(first)
	assert.Equal(t, []string{"ei-obj-detection-runner"}, zone.services)
	assert.NotEmpty(t, zone.fingerprint)

	t.Run("sidecars only, on the shared network", func(t *testing.T) {
		project := loadCompose(t, sidecarsComposeFile(cfg))
		assert.Equal(t, sidecarsProject, project.Name)
		assert.Equal(t, []string{"ei-obj-detection-runner"}, project.ServiceNames())
		sidecar := project.Services["ei-obj-detection-runner"]
		assert.Empty(t, sidecar.DependsOn)
		assert.Equal(t, map[string]string{DockerAppLabel: "true"}, map[string]string(sidecar.Labels), "nothing names the app")
		assert.Equal(t, "bar", *sidecar.Environment["FOO"])
		assert.NotContains(t, sidecar.Environment, "APP_HOME")

		network := project.Networks["default"]
		assert.Equal(t, launcherNetwork, network.Name)
		assert.True(t, bool(network.External))
	})

	t.Run("another app with the same sidecars generates the same zone", func(t *testing.T) {
		before := zoneFile()
		second := newTestApp(t, cfg, "second", "arduino:object_detection", "arduino:custom_ui")
		again := zoneFor(second)
		assert.Equal(t, before, zoneFile())
		assert.Equal(t, zone.services, again.services)
		assert.Equal(t, zone.fingerprint, again.fingerprint)
	})

	t.Run("a sidecar mounting the app folder tells two apps apart", func(t *testing.T) {
		one := newTestApp(t, cfg, "one", "arduino:storage")
		oneZone := zoneFor(one)

		other := newTestApp(t, cfg, "other", "arduino:storage")
		otherZone := zoneFor(other)
		assert.NotEqual(t, oneZone.fingerprint, otherZone.fingerprint, "the resolved volumes differ")
		assert.Contains(t, zoneFile(), other.FullPath.Join("data", "db").String())
		assert.True(t, other.FullPath.Join("data", "db").IsDir(), "the volume folder is pre-provisioned")
	})

	t.Run("a named volume is the zone's, whatever the app", func(t *testing.T) {
		a := newTestApp(t, cfg, "cached", "arduino:cache")
		cached := zoneFor(a)
		assert.Equal(t, sidecarsProject+"_cache-data", cached.project.Volumes["cache-data"].Name)
		assert.NotContains(t, zoneFile(), "cached_cache-data")
	})

	t.Run("no sidecars, no zone files", func(t *testing.T) {
		require.NoError(t, writeSidecarsFingerprint(cfg, zone.fingerprint))
		a := newTestApp(t, cfg, "ui-only", "arduino:web_ui")
		empty := zoneFor(a)
		assert.Empty(t, empty.services)
		assert.True(t, sidecarsComposeFile(cfg).NotExist())
		assert.Empty(t, readSidecarsFingerprint(cfg))
	})
}

func TestSidecarsFingerprintFile(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	require.NoError(t, sidecarsDir(cfg).MkdirAll())
	assert.Empty(t, readSidecarsFingerprint(cfg))
	require.NoError(t, writeSidecarsFingerprint(cfg, "abc"))
	assert.Equal(t, "abc", readSidecarsFingerprint(cfg))
	require.NoError(t, writeSidecarsFingerprint(cfg, ""))
	assert.Empty(t, readSidecarsFingerprint(cfg))
	require.NoError(t, writeSidecarsFingerprint(cfg, ""), "forgetting twice is fine")
}

func TestAppContainers(t *testing.T) {
	zoneContainer := func(service string, state container.ContainerState) container.Summary {
		return container.Summary{ID: service, State: state, Labels: map[string]string{DockerAppLabel: "true", composeProjectLabel: sidecarsProject, composeServiceLabel: service}}
	}
	legacyContainer := container.Summary{ID: "old", State: container.StateRunning, Labels: map[string]string{DockerAppLabel: "true", composeProjectLabel: "my-app", composeServiceLabel: "runner"}}

	zone, legacy := splitAppContainers([]container.Summary{zoneContainer("runner", container.StateRunning), zoneContainer("db", container.StateExited), legacyContainer})
	assert.Equal(t, []container.Summary{legacyContainer}, legacy)
	assert.True(t, zone.cover([]string{"runner", "db"}))
	assert.False(t, zone.cover([]string{"runner", "other"}))
	assert.Equal(t, []string{"db"}, slices.Collect(maps.Keys(zone.notRunning())))
}

func TestGenerateLauncherComposeFile(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	require.NoError(t, cfg.AppsDir().MkdirAll())
	require.NoError(t, cfg.ExamplesBaseDir().MkdirAll())
	require.NoError(t, cfg.RuntimeDir().MkdirAll())
	boardEnvs := types.Mapping{"BOARD_NAME": "unoq", "HOST_IP": "10.0.0.2"}

	changed, err := generateLauncherComposeFile([]int{7100, 8000}, testPythonImage, cfg, boardEnvs, platform.Platform{})
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

	published := make([]uint32, 0, len(svc.Ports))
	for _, p := range svc.Ports {
		published = append(published, p.Target)
	}
	assert.Equal(t, []uint32{7100, 8000}, published)

	assert.Equal(t, types.MappingWithEquals{
		"BOARD_NAME":          new("unoq"),
		"HOST_IP":             new("10.0.0.2"),
		"APP_LAUNCHER_PORTS":  new("7100,8000"),
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
	assert.Equal(t, cfg.RuntimeDir().String(), targets[launcherSocketDir])
	assert.NotContains(t, targets, "/app")

	network := project.Networks["default"]
	assert.Equal(t, launcherNetwork, network.Name)
	assert.False(t, bool(network.External))

	t.Run("a second generation changes nothing", func(t *testing.T) {
		changed, err := generateLauncherComposeFile([]int{7100, 8000}, testPythonImage, cfg, boardEnvs, platform.Platform{})
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
		data, err := launcherComposeFile(cfg).ReadFile()
		require.NoError(t, err)
		assert.Equal(t, "7100,8000,9000", launcherForwardedPorts(data))
	})
}

func TestLauncherPorts(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)
	writeTestApp(t, cfg.AppsDir().Join("with-web-ui"), "name: With Web UI\nbricks:\n- arduino:web_ui: {}\n")
	writeTestApp(t, cfg.ExamplesBaseDir().Join("inspirational", "common", "custom-ui"), "name: Custom UI\nbricks:\n- arduino:custom_ui: {}\n")
	writeTestApp(t, cfg.AppsDir().Join("own-port"), "name: Own Port\nports:\n- 9000\nbricks:\n- arduino:plain: {}\n")

	// The union of the apps, never 9100 of the brick no app uses
	assert.Equal(t, []int{7100, 8000, 9000}, launcherPorts(cfg, bricksIndex))

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
}

func TestUncoveredAppPort(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)
	ui := app.ArduinoApp{Descriptor: app.AppDescriptor{Bricks: []app.Brick{{ID: "arduino:custom_ui"}}}}

	_, uncovered := uncoveredAppPort(ui, bricksIndex, []int{7100, 8000})
	assert.False(t, uncovered)

	port, uncovered := uncoveredAppPort(ui, bricksIndex, []int{7100})
	assert.True(t, uncovered)
	assert.Equal(t, 8000, port)

	ui.Descriptor.Ports = []int{7000}
	port, uncovered = uncoveredAppPort(ui, bricksIndex, []int{7100, 8000})
	assert.True(t, uncovered)
	assert.Equal(t, 7000, port)
}

func TestLauncherWithoutPorts(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)
	require.NoError(t, cfg.AppsDir().MkdirAll())
	require.NoError(t, cfg.RuntimeDir().MkdirAll())

	assert.Empty(t, launcherPorts(cfg, bricksIndex), "no app, no port")

	_, err := generateLauncherComposeFile(nil, testPythonImage, cfg, nil, platform.Platform{})
	require.NoError(t, err)
	data, err := launcherComposeFile(cfg).ReadFile()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "ports:", "no ingress")
	svc := loadCompose(t, launcherComposeFile(cfg)).Services[launcherServiceName]
	assert.Empty(t, svc.Ports)
	assert.Equal(t, "", *svc.Environment["APP_LAUNCHER_PORTS"])
}

func TestLauncherUpReason(t *testing.T) {
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
			reason, up := launcherUpReason(tc.forceUp, tc.changed, tc.running)
			assert.Equal(t, tc.wantUp, up)
			assert.Equal(t, tc.wantUp, reason != "")
		})
	}
}

func TestPrepareLauncherRejectsWhatItCannotRun(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	bricksIndex, _ := setTestBricks(t, cfg)
	noop := func(StreamMessage) {}

	// Changed after the launcher ports were collected: not on disk
	changed := newTestApp(t, cfg, "changed")
	changed.Descriptor.Ports = []int{8080}
	err := prepareLauncher(t.Context(), nil, bricksIndex, changed, cfg, platform.Platform{}, noop)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not forward port 8080")

	elsewhere := app.ArduinoApp{Name: "elsewhere", FullPath: paths.New(t.TempDir())}
	err = prepareLauncher(t.Context(), nil, bricksIndex, elsewhere, cfg, platform.Platform{}, noop)
	require.Error(t, err)
	assert.Contains(t, err.Error(), cfg.AppsDir().String())
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
