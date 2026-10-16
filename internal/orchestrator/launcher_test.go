// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bufio"
	"maps"
	"os"
	"slices"
	"strings"
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
		assert.Equal(t, launcherNetwork, network.Name)
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

	zone, legacy := splitAppContainers([]container.Summary{zoneContainer("runner", container.StateRunning), zoneContainer("db", container.StateExited), legacyContainer})
	assert.Equal(t, []container.Summary{legacyContainer}, legacy)
	assert.True(t, zone.cover([]string{"runner", "db"}))
	assert.False(t, zone.cover([]string{"runner", "other"}))
	assert.Equal(t, []string{"db"}, slices.Collect(maps.Keys(zone.notRunning())))
	assert.Equal(t, []string{"runner"}, slices.Collect(maps.Keys(zone.only([]string{"runner", "other"}))))
	assert.Equal(t, []container.Summary{zone["db"]}, zone.orphans([]string{"runner"}))
}

func TestGenerateSlotComposeFile(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	boardEnvs := types.Mapping{"BOARD_NAME": "unoq", "HOST_IP": "10.0.0.2"}
	s := slot{ID: 7}
	require.NoError(t, s.dir(cfg).MkdirAll())
	require.NoError(t, s.mountpoint(cfg).MkdirAll())
	require.NoError(t, s.socketDir(cfg).MkdirAll())

	require.NoError(t, generateSlotComposeFile(testPythonImage, cfg, boardEnvs, platform.Platform{}, s))

	project := loadCompose(t, s.composeFile(cfg))
	assert.Equal(t, "arduino-app-slot-7", project.Name)
	assert.Equal(t, []string{"slot7"}, project.ServiceNames(), "a name of its own on the network, the ingress upstream")
	svc := project.Services["slot7"]

	assert.Equal(t, testPythonImage, svc.Image)
	assert.Equal(t, types.ShellCommand{"/run.sh", "serve"}, svc.Entrypoint)
	assert.True(t, *svc.Init)
	assert.Empty(t, svc.Restart, "a slot that stops is replaced, not restarted")
	assert.Equal(t, map[string]string{DockerLauncherLabel: "true", DockerSlotLabel: "7"}, map[string]string(svc.Labels))
	require.NotNil(t, svc.HealthCheck)
	assert.Equal(t, types.HealthCheckTest{"CMD", "arduino-app-launcher", "ping"}, svc.HealthCheck.Test)
	assert.Empty(t, svc.Ports, "the ingress publishes the app ports")

	assert.Equal(t, types.MappingWithEquals{
		"BOARD_NAME":          new("unoq"),
		"HOST_IP":             new("10.0.0.2"),
		"APP_HOME":            new("/app"),
		"APP_LAUNCHER_SOCKET": new("/run/arduino-app-cli/app-launcher.sock"),
		// The CLI waits for a start as long as the launcher may prepare it
		"APP_LAUNCHER_PREPARE_TIMEOUT_S": new("600"),
	}, svc.Environment)

	volumes := make(map[string]types.ServiceVolumeConfig, len(svc.Volumes))
	for _, v := range svc.Volumes {
		volumes[v.Target] = v
	}
	require.Contains(t, volumes, "/app")
	assert.Equal(t, s.mountpoint(cfg).String(), volumes["/app"].Source)
	require.NotNil(t, volumes["/app"].Bind)
	assert.Equal(t, "rslave", volumes["/app"].Bind.Propagation, "the mirror mounted later shows inside")
	assert.Equal(t, s.socketDir(cfg).String(), volumes[launcherSocketDir].Source, "its own socket folder only")
	for _, root := range launcherAppRoots(cfg) {
		assert.NotContains(t, volumes, root.String(), "no apps tree is mounted")
	}

	network := project.Networks["default"]
	assert.Equal(t, launcherNetwork, network.Name)
	assert.True(t, bool(network.External))

	t.Run("a preload override reaches the launcher", func(t *testing.T) {
		cfg.LauncherPreload = "arduino.app_utils,arduino.app_peripherals.camera"
		require.NoError(t, generateSlotComposeFile(testPythonImage, cfg, boardEnvs, platform.Platform{}, s))
		svc := loadCompose(t, s.composeFile(cfg)).Services["slot7"]
		assert.Equal(t, new("arduino.app_utils,arduino.app_peripherals.camera"), svc.Environment["APP_LAUNCHER_PRELOAD"])
	})
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

func TestCheckLauncherCanRun(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)

	elsewhere := app.ArduinoApp{Name: "elsewhere", FullPath: paths.New(t.TempDir())}
	err := checkLauncherCanRun(elsewhere, nil, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), cfg.AppsDir().String())

	// Not a TCP port
	high := newTestApp(t, cfg, "high")
	err = checkLauncherCanRun(high, []int{8000, 70000}, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot publish port 70000")

	assert.NoError(t, checkLauncherCanRun(high, []int{7000}, cfg))
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

func TestSlotRecords(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	first, second := cfg.AppsDir().Join("first"), cfg.AppsDir().Join("second")

	assert.Empty(t, readSlots(cfg))
	_, ok := activeSlot(cfg, nil)
	assert.False(t, ok)

	spare, err := newSlotDir(cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, spare.ID)
	assert.Empty(t, readSlots(cfg), "a slot without record is still warming, never taken")
	require.NoError(t, writeSlot(cfg, spare))

	next, err := newSlotDir(cfg)
	require.NoError(t, err)
	assert.Equal(t, 2, next.ID, "ids are not reused while a folder exists")
	writeTestSlot(t, cfg, slot{ID: next.ID, App: first.String(), FSPid: 42})

	got, ok := activeSlot(cfg, first)
	require.True(t, ok)
	assert.Equal(t, slot{ID: 2, App: first.String(), FSPid: 42}, got)
	_, ok = activeSlot(cfg, second)
	assert.False(t, ok, "another app has no slot")

	got.Retired = true
	writeTestSlot(t, cfg, got)
	_, ok = activeSlot(cfg, nil)
	assert.False(t, ok, "a retired slot runs nothing")
	assert.Equal(t, []slot{{ID: 1}, {ID: 2, App: first.String(), Retired: true, FSPid: 42}}, readSlots(cfg))
}

func TestParseSlotFSMounts(t *testing.T) {
	mountinfo := `22 1 179:68 / / rw,relatime shared:1 - ext4 /dev/mmcblk0p68 rw
90 22 0:61 / /var/lib/arduino-app-cli/slots/3/app rw,nosuid,nodev,relatime shared:70 - fuse.slotfs /home/arduino/ArduinoApps/blink rw,user_id=1000,group_id=1000
91 22 0:62 / /var/lib/arduino-app-cli/slots/4/my\040app rw,nosuid,nodev - fuse.slotfs /home/arduino/ArduinoApps/my\040app rw,user_id=1000
92 22 0:63 / /tmp/other rw,nosuid,nodev - fuse.sshfs host:/ rw
`
	assert.Equal(t, []string{"/var/lib/arduino-app-cli/slots/3/app", "/var/lib/arduino-app-cli/slots/4/my app"},
		parseSlotFSMounts(bufio.NewScanner(strings.NewReader(mountinfo))))
}

// writeTestSlot records a slot, its folder created.
func writeTestSlot(t *testing.T, cfg config.Configuration, s slot) {
	t.Helper()
	require.NoError(t, s.dir(cfg).MkdirAll())
	require.NoError(t, writeSlot(cfg, s))
}

func TestSlotSettleSignals(t *testing.T) {
	t.Run("the unified hierarchy path of a container", func(t *testing.T) {
		path, ok := cgroupV2Path("0::/system.slice/docker-0123abcd.scope\n")
		assert.True(t, ok)
		assert.Equal(t, "/system.slice/docker-0123abcd.scope", path)
		_, ok = cgroupV2Path("12:cpu,cpuacct:/docker/0123abcd\n")
		assert.False(t, ok, "cgroup v1 is not read")
	})

	t.Run("the CPU time of a cgroup", func(t *testing.T) {
		stat := paths.New(t.TempDir(), "cpu.stat")
		require.NoError(t, stat.WriteFile([]byte("usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n")))
		usage, err := readCPUUsage(stat.String())
		require.NoError(t, err)
		assert.Equal(t, 1500*time.Millisecond, usage)
	})

	t.Run("quiet below a sixth of a core", func(t *testing.T) {
		assert.True(t, isQuiet(10*time.Millisecond, 200*time.Millisecond))
		assert.False(t, isQuiet(80*time.Millisecond, 200*time.Millisecond), "an app still importing")
	})
}
