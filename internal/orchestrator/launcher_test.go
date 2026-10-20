// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
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
	assert.Equal(t, []string{"ei-obj-detection-runner"}, slices.Sorted(maps.Keys(zone.fingerprints)))

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
		assert.Equal(t, appsNetwork, network.Name)
		assert.True(t, bool(network.External))
	})

	t.Run("another app with the same sidecars generates the same zone", func(t *testing.T) {
		before := zoneFile()
		second := newTestApp(t, cfg, "second", "arduino:object_detection", "arduino:custom_ui")
		again := zoneFor(second)
		assert.Equal(t, before, zoneFile())
		assert.Equal(t, zone.services, again.services)
		assert.Equal(t, zone.fingerprints, again.fingerprints)
	})

	t.Run("a sidecar mounting the app folder tells two apps apart", func(t *testing.T) {
		one := newTestApp(t, cfg, "one", "arduino:storage")
		oneZone := zoneFor(one)

		other := newTestApp(t, cfg, "other", "arduino:storage")
		otherZone := zoneFor(other)
		assert.NotEqual(t, oneZone.fingerprints["storage-db"], otherZone.fingerprints["storage-db"], "the resolved volumes differ")
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
		a := newTestApp(t, cfg, "ui-only", "arduino:web_ui")
		empty := zoneFor(a)
		assert.Empty(t, empty.services)
		assert.Equal(t, map[string]string{}, empty.fingerprints, "an empty zone, not an unresolved one")
		assert.True(t, sidecarsComposeFile(cfg).NotExist())
		assert.Nil(t, readSidecarsRecord(cfg), "the record is left to the reconcile")
	})
}

func TestSidecarsRecord(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	assert.Nil(t, readSidecarsRecord(cfg))
	require.NoError(t, writeSidecarsRecord(cfg, map[string]string{"runner": "abc"}))
	assert.Equal(t, map[string]string{"runner": "abc"}, readSidecarsRecord(cfg))
	require.NoError(t, writeSidecarsRecord(cfg, map[string]string{}))
	assert.Equal(t, map[string]string{}, readSidecarsRecord(cfg), "an empty zone is recorded")
	require.NoError(t, writeSidecarsRecord(cfg, nil))
	assert.Nil(t, readSidecarsRecord(cfg))
	require.NoError(t, writeSidecarsRecord(cfg, nil), "forgetting twice is fine")
}

func TestPlanSidecars(t *testing.T) {
	running := container.Summary{State: container.StateRunning}
	exited := container.Summary{State: container.StateExited}
	recorded := map[string]string{"runner": "r1", "db": "d1"}
	both := appContainers{"runner": running, "db": running}

	for _, tc := range []struct {
		name       string
		zone       sidecarsZone
		recorded   map[string]string
		containers appContainers
		want       sidecarsPlan
	}{
		{name: "unchanged and running", zone: sidecarsZone{services: []string{"db", "runner"}, fingerprints: recorded}, recorded: recorded, containers: both, want: sidecarsKeep},
		{name: "a subset of what runs", zone: sidecarsZone{services: []string{"runner"}, fingerprints: map[string]string{"runner": "r1"}}, recorded: recorded, containers: both, want: sidecarsKeep},
		{name: "no sidecars", zone: sidecarsZone{fingerprints: map[string]string{}}, recorded: recorded, containers: both, want: sidecarsKeep},
		{name: "unchanged and stopped", zone: sidecarsZone{services: []string{"runner"}, fingerprints: map[string]string{"runner": "r1"}}, recorded: recorded, containers: appContainers{"runner": exited}, want: sidecarsStart},
		{name: "a changed service", zone: sidecarsZone{services: []string{"runner"}, fingerprints: map[string]string{"runner": "r2"}}, recorded: recorded, containers: both, want: sidecarsUp},
		{name: "a new service", zone: sidecarsZone{services: []string{"runner", "web"}, fingerprints: map[string]string{"runner": "r1", "web": "w1"}}, recorded: recorded, containers: both, want: sidecarsUp},
		{name: "a removed container", zone: sidecarsZone{services: []string{"db"}, fingerprints: map[string]string{"db": "d1"}}, recorded: recorded, containers: appContainers{"runner": running}, want: sidecarsUp},
		{name: "no record", zone: sidecarsZone{services: []string{"runner"}, fingerprints: map[string]string{"runner": "r1"}}, containers: both, want: sidecarsUp},
		{name: "an unresolved zone", zone: sidecarsZone{services: []string{"runner"}}, recorded: recorded, containers: both, want: sidecarsUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, planSidecars(tc.zone, tc.recorded, tc.containers))
		})
	}
}

func TestAppContainers(t *testing.T) {
	zoneContainer := func(service string, state container.ContainerState) container.Summary {
		return container.Summary{ID: service, State: state, Labels: map[string]string{DockerAppLabel: "true", composeProjectLabel: sidecarsProject, composeServiceLabel: service}}
	}
	legacyContainer := container.Summary{ID: "old", State: container.StateRunning, Labels: map[string]string{DockerAppLabel: "true", composeProjectLabel: "my-app", composeServiceLabel: "runner"}}
	appContainer := container.Summary{ID: "app", State: container.StateRunning, Labels: map[string]string{DockerAppLabel: "true", DockerAppMainLabel: "true", composeProjectLabel: "my-app", composeServiceLabel: mainServiceName}}

	zone, legacy := splitAppContainers([]container.Summary{zoneContainer("runner", container.StateRunning), zoneContainer("db", container.StateExited), legacyContainer, appContainer})
	assert.Equal(t, []container.Summary{legacyContainer}, legacy)
	assert.True(t, zone.cover([]string{"runner", "db"}))
	assert.False(t, zone.cover([]string{"runner", "other"}))
	assert.Equal(t, []string{"db"}, slices.Collect(maps.Keys(zone.notRunning())))
	assert.Equal(t, []string{"runner"}, slices.Collect(maps.Keys(zone.only([]string{"runner", "other"}))))
	assert.Equal(t, []container.Summary{zone["db"]}, zone.orphans([]string{"runner"}))
}

func TestGenerateAppComposeFile(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	a := newTestApp(t, cfg, "my-app", "arduino:web_ui")
	c := appContainerOf(cfg, a.FullPath)
	boardEnvs := types.Mapping{"BOARD_NAME": "unoq", "HOST_IP": "10.0.0.2"}

	changed, err := generateAppComposeFile(c, a.FullPath, []int{7000, 8080}, testPythonImage, cfg, boardEnvs, platform.Platform{})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, cfg.DataDir().Join("apps", "my-app", "compose.yaml").String(), c.composeFile(cfg).String())

	project := loadCompose(t, c.composeFile(cfg))
	assert.Equal(t, "my-app", project.Name)
	assert.Equal(t, []string{mainServiceName}, project.ServiceNames())
	svc := project.Services[mainServiceName]

	assert.Equal(t, testPythonImage, svc.Image)
	assert.Equal(t, types.ShellCommand{"/run.sh", "serve"}, svc.Entrypoint)
	assert.Equal(t, "/app", svc.WorkingDir)
	assert.True(t, *svc.Init)
	assert.Equal(t, "no", svc.Restart)
	assert.Equal(t, types.Duration(10*time.Second), *svc.StopGracePeriod)
	assert.Equal(t, map[string]string{DockerAppLabel: "true", DockerAppMainLabel: "true", DockerAppPathLabel: a.FullPath.String()}, map[string]string(svc.Labels))
	assert.Nil(t, svc.HealthCheck, "a healthcheck would run a python in every warm container")
	published := make([]string, 0, len(svc.Ports))
	for _, p := range svc.Ports {
		published = append(published, fmt.Sprintf("%s:%d/%s", p.Published, p.Target, p.Protocol))
	}
	assert.Equal(t, []string{"7000:7000/tcp", "8080:8080/tcp"}, published, "the container publishes the app ports")

	assert.Equal(t, types.MappingWithEquals{
		"BOARD_NAME": new("unoq"),
		"HOST_IP":    new("10.0.0.2"),
		// The CLI waits for a start as long as the supervisor may prepare it
		"APP_LAUNCHER_PREPARE_TIMEOUT_S": new("600"),
	}, svc.Environment, "nothing of the app: its environment goes with each start")

	targets := make(map[string]string, len(svc.Volumes))
	for _, v := range svc.Volumes {
		targets[v.Target] = v.Source
	}
	assert.Equal(t, a.FullPath.String(), targets["/app"])
	assert.Equal(t, cfg.RuntimeDir().Join("apps", "my-app").String(), targets["/run/arduino-app-cli"])
	assert.Equal(t, "/dev", targets["/dev"])

	network := project.Networks["default"]
	assert.Equal(t, appsNetwork, network.Name)
	assert.True(t, bool(network.External))
	assert.Equal(t, []string{"my-app-main-1"}, svc.Networks["default"].Aliases)

	t.Run("a second generation changes nothing", func(t *testing.T) {
		changed, err := generateAppComposeFile(c, a.FullPath, []int{7000, 8080}, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("new ports change it", func(t *testing.T) {
		changed, err := generateAppComposeFile(c, a.FullPath, []int{7000}, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("another board environment changes it", func(t *testing.T) {
		changed, err := generateAppComposeFile(c, a.FullPath, []int{7000}, testPythonImage, cfg, types.Mapping{"HOST_IP": "10.0.0.3"}, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("a preload override reaches the container and changes the file", func(t *testing.T) {
		cfg := cfg
		cfg.LauncherPreload = "arduino.app_utils,arduino.app_peripherals.camera"
		changed, err := generateAppComposeFile(c, a.FullPath, []int{7000}, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		assert.True(t, changed)
		svc := loadCompose(t, c.composeFile(cfg)).Services[mainServiceName]
		assert.Equal(t, new("arduino.app_utils,arduino.app_peripherals.camera"), svc.Environment["APP_LAUNCHER_PRELOAD"])
	})

	t.Run("a $ in the app path is kept", func(t *testing.T) {
		odd := app.ArduinoApp{FullPath: paths.New(t.TempDir(), "price$tag")}
		c := appContainerOf(cfg, odd.FullPath)
		_, err := generateAppComposeFile(c, odd.FullPath, nil, testPythonImage, cfg, boardEnvs, platform.Platform{})
		require.NoError(t, err)
		svc := loadCompose(t, c.composeFile(cfg)).Services[mainServiceName]
		assert.Equal(t, odd.FullPath.String(), svc.Labels[DockerAppPathLabel])
	})
}

func TestAppContainerNames(t *testing.T) {
	short := appContainer{project: "my-app"}
	assert.Equal(t, "my-app-main-1", short.host(), "the compose name of the container")
	assert.Equal(t, "my-app", short.key())

	// An example of the board, named after its path from the apps folder
	long := appContainer{project: composeProjectName(
		paths.New("/var/lib/arduino-app-cli/examples/inspirational/common/a-rather-long-example-name"),
		paths.New("/home/arduino/ArduinoApps"),
	)}
	host := long.host()
	assert.LessOrEqual(t, len(host), 63, "a DNS label")
	assert.Regexp(t, `^[a-z0-9-]+[a-z0-9]$`, host)
	assert.NotEqual(t, host, appContainer{project: long.project + "x"}.host(), "unique per project")
	assert.Equal(t, host, long.host())
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

func TestLoadAppContainerSpec(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	a := newTestApp(t, cfg, "my-app")
	c := appContainerOf(cfg, a.FullPath)
	_, err := generateAppComposeFile(c, a.FullPath, []int{7000}, testPythonImage, cfg, types.Mapping{"BOARD_NAME": "unoq"}, platform.Platform{})
	require.NoError(t, err)

	spec, err := loadAppContainerSpec(t.Context(), cfg, c, "abc")
	require.NoError(t, err)
	assert.Equal(t, "my-app-main-1", c.containerName())

	assert.Equal(t, testPythonImage, spec.config.Image)
	assert.Equal(t, []string{"/run.sh", "serve"}, spec.config.Entrypoint)
	assert.Equal(t, "/app", spec.config.WorkingDir)
	assert.Equal(t, 10, *spec.config.StopTimeout)
	assert.ElementsMatch(t, []string{"BOARD_NAME=unoq", "APP_LAUNCHER_PREPARE_TIMEOUT_S=600"}, spec.config.Env)
	assert.Equal(t, map[string]string{
		DockerAppLabel:     "true",
		DockerAppMainLabel: "true",
		DockerAppPathLabel: a.FullPath.String(),
		appDefinitionLabel: "abc",
		// What compose stamps, so that compose and the sidecars split find the container
		composeProjectLabel:                       "my-app",
		composeServiceLabel:                       mainServiceName,
		"com.docker.compose.config-hash":          "abc",
		"com.docker.compose.version":              api.ComposeVersion,
		"com.docker.compose.container-number":     "1",
		"com.docker.compose.oneoff":               "False",
		"com.docker.compose.project.working_dir":  c.dir(cfg).String(),
		"com.docker.compose.project.config_files": c.composeFile(cfg).String(),
	}, spec.config.Labels)

	assert.True(t, *spec.host.Init)
	assert.Equal(t, container.RestartPolicyDisabled, spec.host.RestartPolicy.Name)
	assert.Equal(t, container.NetworkMode(appsNetwork), spec.host.NetworkMode)
	assert.Equal(t, []string{"msgpack-rpc-router:host-gateway"}, spec.host.ExtraHosts)
	assert.Equal(t, "json-file", spec.host.LogConfig.Type)
	tcp7000 := network.MustParsePort("7000/tcp")
	assert.Equal(t, network.PortSet{tcp7000: {}}, spec.config.ExposedPorts)
	assert.Equal(t, network.PortMap{tcp7000: {{HostPort: "7000"}}}, spec.host.PortBindings)
	targets := map[string]string{}
	for _, m := range spec.host.Mounts {
		targets[m.Target] = m.Source
	}
	assert.Equal(t, a.FullPath.String(), targets["/app"])
	assert.Equal(t, c.runtimeDir(cfg).String(), targets["/run/arduino-app-cli"])
	assert.Equal(t, []string{"my-app-main-1"}, spec.networking.EndpointsConfig[appsNetwork].Aliases)
}
