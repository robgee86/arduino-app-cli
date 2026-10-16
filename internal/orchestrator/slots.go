// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	yaml "github.com/goccy/go-yaml"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/pipewire"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

// A slot is a python-apps-base container running the app launcher, warmed before the app it serves
// is known. Its /app is an empty folder of the CLI until a start mirrors the app folder onto it
// through FUSE, so that it never sees another app. A slot serves one app and is removed after it.
const (
	DockerSlotLabel = "cc.arduino.app.slot"

	slotProjectPrefix = "arduino-app-slot-"
	slotAppTarget     = "/app"
	// slotFSType is how the mirror shows in mountinfo
	slotFSType = "fuse.slotfs"
	// slotAttachTimeout bounds the wait for the mirror to be mounted
	slotAttachTimeout = 5 * time.Second
	// SlotFSCacheTimeout is how long the kernel trusts what it read through the mirror: an edit made
	// on the host shows in the slot after it, which a reload right after the edit relies on.
	SlotFSCacheTimeout = time.Second
)

// slot is the record of a slot, kept next to its folders so that every CLI process sees it.
type slot struct {
	ID int `json:"id"`
	// App is the host folder of the app the slot serves, empty for a spare.
	App string `json:"app,omitempty"`
	// Retired marks a slot whose app stopped: it is never used again and goes away.
	Retired bool `json:"retired,omitempty"`
	// FSUnit is the user unit serving the mirror of App, FSPid its process when no user manager runs.
	FSUnit string `json:"fs_unit,omitempty"`
	FSPid  int    `json:"fs_pid,omitempty"`
}

func slotsDir(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("slots")
}

func (s slot) dir(cfg config.Configuration) *paths.Path {
	return slotsDir(cfg).Join(strconv.Itoa(s.ID))
}

// mountpoint is the host folder the container sees as /app.
func (s slot) mountpoint(cfg config.Configuration) *paths.Path {
	return s.dir(cfg).Join("app")
}

func (s slot) recordFile(cfg config.Configuration) *paths.Path {
	return s.dir(cfg).Join("slot.json")
}

func (s slot) composeFile(cfg config.Configuration) *paths.Path {
	return s.dir(cfg).Join("compose.yaml")
}

// socketDir holds the control socket of the slot only, never the one of another slot.
func (s slot) socketDir(cfg config.Configuration) *paths.Path {
	return cfg.RuntimeDir().Join("slots", strconv.Itoa(s.ID))
}

func (s slot) project() string {
	return slotProjectPrefix + strconv.Itoa(s.ID)
}

// service is the name of the slot on the shared network, the ingress upstream.
func (s slot) service() string {
	return "slot" + strconv.Itoa(s.ID)
}

// mirrorUnit is the user unit serving the app folder mirror of the slot.
func (s slot) mirrorUnit() string {
	return slotProjectPrefix + strconv.Itoa(s.ID) + "-fs.service"
}

func (s slot) client(cfg config.Configuration) *launcher.Client {
	return launcher.New(s.socketDir(cfg).Join(launcher.SocketName).String(), launcherRequestTimeout)
}

func (s slot) active() bool {
	return s.App != "" && !s.Retired
}

// readSlots returns the slots with a record, by id. A slot is recorded once warm.
func readSlots(cfg config.Configuration) []slot {
	entries, err := slotsDir(cfg).ReadDir()
	if err != nil {
		return nil
	}
	var slots []slot
	for _, entry := range entries {
		id, err := strconv.Atoi(entry.Base())
		if err != nil {
			continue
		}
		data, err := (slot{ID: id}).recordFile(cfg).ReadFile()
		if err != nil {
			continue
		}
		var s slot
		if err := json.Unmarshal(data, &s); err != nil || s.ID != id {
			slog.Warn("ignoring an unreadable slot record", slog.Int("slot", id))
			continue
		}
		slots = append(slots, s)
	}
	slices.SortFunc(slots, func(a, b slot) int { return a.ID - b.ID })
	return slots
}

func writeSlot(cfg config.Configuration, s slot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return fatomic.WriteFile(s.recordFile(cfg).String(), data, 0o644)
}

// activeSlot is the slot serving appPath, or any app when appPath is nil.
func activeSlot(cfg config.Configuration, appPath *paths.Path) (slot, bool) {
	for _, s := range readSlots(cfg) {
		if s.active() && (appPath == nil || paths.New(s.App).EqualsTo(appPath)) {
			return s, true
		}
	}
	return slot{}, false
}

// newSlotDir reserves the next free slot id by creating its folder.
func newSlotDir(cfg config.Configuration) (slot, error) {
	if err := slotsDir(cfg).MkdirAll(); err != nil {
		return slot{}, err
	}
	next := 1
	if entries, err := slotsDir(cfg).ReadDir(); err == nil {
		for _, entry := range entries {
			if id, err := strconv.Atoi(entry.Base()); err == nil && id >= next {
				next = id + 1
			}
		}
	}
	for ; ; next++ {
		s := slot{ID: next}
		err := os.Mkdir(s.dir(cfg).String(), 0o755)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return slot{}, err
		}
	}
}

// generateSlotComposeFile writes the compose file of a slot: the launcher of the board with its own
// socket folder and an empty /app, nothing of the apps trees.
func generateSlotComposeFile(pythonImage string, cfg config.Configuration, boardEnvs types.Mapping, platform platform.Platform, s slot) error {
	envs := boardEnvs.Clone()
	envs["APP_LAUNCHER_SOCKET"] = launcherSocketDir + "/" + launcher.SocketName
	envs["APP_LAUNCHER_PREPARE_TIMEOUT_S"] = strconv.Itoa(int(launcherPrepareTimeout.Seconds()))
	envs[appHomeVar] = slotAppTarget
	if cfg.LauncherPreload != "" {
		envs["APP_LAUNCHER_PRELOAD"] = cfg.LauncherPreload
	}
	runtime, err := newPythonRuntime(cfg, platform)
	if err != nil {
		return err
	}
	svc := runtime.service(pythonImage,
		volume{Type: "bind", Source: s.socketDir(cfg).String(), Target: launcherSocketDir},
		// rslave: the mirror mounted on the folder later shows inside the running container
		volume{Type: "bind", Source: s.mountpoint(cfg).String(), Target: slotAppTarget, Bind: &bindOptions{Propagation: "rslave"}},
	)
	svc.Entrypoint = "/run.sh serve"
	svc.StopGracePeriod = "10s"
	svc.Healthcheck = &healthcheck{
		Test:          []string{"CMD", "arduino-app-launcher", "ping"},
		Interval:      "2s",
		StartPeriod:   "60s",
		StartInterval: "500ms",
	}
	svc.Labels = map[string]string{DockerLauncherLabel: "true", DockerSlotLabel: strconv.Itoa(s.ID)}
	svc.Environment = envs

	compose := struct {
		Name     string                    `yaml:"name"`
		Services map[string]service        `yaml:"services"`
		Networks map[string]composeNetwork `yaml:"networks"`
	}{
		Name:     s.project(),
		Services: map[string]service{s.service(): svc},
		Networks: map[string]composeNetwork{"default": {Name: launcherNetwork, External: true}},
	}
	template, err := yaml.Marshal(compose)
	if err != nil {
		return err
	}
	data, err := renderComposeTemplate(template)
	if err != nil {
		return fmt.Errorf("failed to render the slot compose file: %w", err)
	}
	return fatomic.WriteFile(s.composeFile(cfg).String(), data, 0o644)
}

// slotContainer is the container of the slot, nil without one.
func slotContainer(ctx context.Context, docker dockerClient.APIClient, s slot) (*container.Summary, error) {
	containers, err := dockerhelper.Containers(ctx, docker, DockerSlotLabel+"="+strconv.Itoa(s.ID))
	if err != nil || len(containers) == 0 {
		return nil, err
	}
	return &containers[0], nil
}

// createSlot brings a new spare up and returns it once its launcher answers.
func createSlot(ctx context.Context, docker command.Cli, cfg config.Configuration, platform platform.Platform) (slot, error) {
	started := time.Now()
	s, err := newSlotDir(cfg)
	if err != nil {
		return slot{}, fmt.Errorf("failed to create a slot folder: %w", err)
	}
	fail := func(err error) (slot, error) {
		removeSlot(context.WithoutCancel(ctx), docker.Client(), cfg, s)
		return slot{}, err
	}
	if err := errors.Join(s.mountpoint(cfg).MkdirAll(), s.socketDir(cfg).MkdirAll()); err != nil {
		return fail(fmt.Errorf("failed to create the slot folders: %w", err))
	}
	if err := generateSlotComposeFile(cfg.PythonImage, cfg, boardEnvironment(ctx, platform, cfg), platform, s); err != nil {
		return fail(err)
	}
	if err := composeUpFile(ctx, docker, s.composeFile(cfg), func(StreamMessage) {}); err != nil {
		return fail(fmt.Errorf("failed to start a slot: %w", err))
	}
	c, err := slotContainer(ctx, docker.Client(), s)
	if err != nil || c == nil {
		return fail(errors.Join(errors.New("the slot container was not created"), err))
	}
	if err := waitLauncherReady(ctx, docker.Client(), s.client(cfg), c.ID); err != nil {
		return fail(err)
	}
	if err := writeSlot(cfg, s); err != nil {
		return fail(err)
	}
	slog.Info("slot ready", slog.Int("slot", s.ID), slog.Duration("elapsed", time.Since(started)))
	return s, nil
}

// removeSlot stops what the slot runs, removes its container, unmounts the mirror and deletes its
// folders. Best effort: what is left is cleared at the next boot.
func removeSlot(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, s slot) {
	started := time.Now()
	if s.App != "" && !s.Retired {
		if _, err := s.client(cfg).WithTimeout(launcherStopTimeout).Stop(ctx, ""); err != nil {
			slog.Debug("slot launcher unreachable, nothing to stop", slog.Int("slot", s.ID), slog.String("error", err.Error()))
		}
	}
	if c, err := slotContainer(ctx, docker, s); err == nil && c != nil {
		if _, err := docker.ContainerRemove(ctx, c.ID, dockerClient.ContainerRemoveOptions{Force: true}); err != nil {
			slog.Warn("failed to remove a slot container", slog.Int("slot", s.ID), slog.String("error", err.Error()))
		}
	}
	detachAppFolder(cfg, s)
	if err := errors.Join(s.dir(cfg).RemoveAll(), s.socketDir(cfg).RemoveAll()); err != nil {
		slog.Warn("failed to remove the slot folders", slog.Int("slot", s.ID), slog.String("error", err.Error()))
	}
	slog.Debug("slot removed", slog.Int("slot", s.ID), slog.Duration("elapsed", time.Since(started)))
}

// attachAppFolder mirrors the app folder onto the slot's /app through FUSE and returns once the
// mirror is mounted. The mirror is served by this executable in a unit of the user's systemd
// manager, so that it lives as long as the app, whoever started it and whatever happens to the
// daemon; without a user manager, in a process of its own. fusermount3 mounts it for the user, who
// owns the mount point.
func attachAppFolder(ctx context.Context, cfg config.Configuration, s *slot, appPath *paths.Path) error {
	started := time.Now()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	serve := []string{exe, "system", "slot-fs", "--cache-timeout", cfg.SlotFSCacheTimeout.String(), appPath.String(), s.mountpoint(cfg).String()}
	exited := make(chan error, 1)
	if hasUserManager() {
		if err := pipewire.EnsureLingering(ctx); err != nil {
			slog.Warn("lingering is off: the app folder mirrors end with the last login session", slog.String("error", err.Error()))
		}
		//nolint:gosec // this executable and folders the CLI resolved itself
		cmd := exec.CommandContext(ctx, "systemd-run", append([]string{"--user", "--quiet", "--collect",
			"--unit=" + s.mirrorUnit(), "--description=App folder mirror of slot " + strconv.Itoa(s.ID), "--"}, serve...)...)
		cmd.Env = append(os.Environ(), userRuntimeDirEnv())
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to start the app folder mirror: %w: %s", err, strings.TrimSpace(string(out)))
		}
		s.FSUnit = s.mirrorUnit()
	} else {
		logFile, err := os.Create(s.dir(cfg).Join("slotfs.log").String())
		if err != nil {
			return err
		}
		defer logFile.Close()
		cmd := exec.Command(serve[0], serve[1:]...) //nolint:gosec // this executable and folders the CLI resolved itself
		cmd.Stdout, cmd.Stderr = logFile, logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start the app folder mirror: %w", err)
		}
		go func() { exited <- cmd.Wait() }()
		s.FSPid = cmd.Process.Pid
	}
	mountpoint := s.mountpoint(cfg).String()
	deadline := time.Now().Add(slotAttachTimeout)
	for !isSlotFSMounted(mountpoint) {
		select {
		case err := <-exited:
			out, _ := s.dir(cfg).Join("slotfs.log").ReadFile()
			return fmt.Errorf("the app folder mirror exited: %v: %s", err, strings.TrimSpace(string(out)))
		case <-time.After(2 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			detachAppFolder(cfg, *s)
			return fmt.Errorf("the app folder mirror was not mounted within %s, see `journalctl --user -u %s`", slotAttachTimeout, s.mirrorUnit())
		}
	}
	slog.Debug("app folder attached", slog.Int("slot", s.ID), slog.String("app", appPath.String()), slog.String("unit", s.FSUnit), slog.Duration("elapsed", time.Since(started)))
	return nil
}

// detachAppFolder unmounts the mirror of the slot, which ends what serves it.
func detachAppFolder(cfg config.Configuration, s slot) {
	mountpoint := s.mountpoint(cfg).String()
	if isSlotFSMounted(mountpoint) {
		if out, err := exec.Command("fusermount3", "-u", mountpoint).CombinedOutput(); err != nil {
			slog.Debug("unmount refused, detaching lazily", slog.String("mountpoint", mountpoint), slog.String("output", strings.TrimSpace(string(out))))
			_ = exec.Command("fusermount3", "-uz", mountpoint).Run()
		}
	}
	switch {
	case s.FSUnit != "":
		_ = userSystemctl(context.Background(), "stop", s.FSUnit)
	case s.FSPid > 0 && isSlotFSProcess(s.FSPid):
		_ = syscall.Kill(s.FSPid, syscall.SIGTERM)
	}
}

// hasUserManager tells whether the user's systemd --user manager runs, from its private socket.
func hasUserManager() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	return paths.New(userRuntimeDir(), "systemd", "private").Exist()
}

func userRuntimeDir() string {
	return "/run/user/" + strconv.Itoa(os.Getuid())
}

func userRuntimeDirEnv() string {
	return "XDG_RUNTIME_DIR=" + userRuntimeDir()
}

// userSystemctl runs systemctl on the user's manager.
func userSystemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...) //nolint:gosec // unit names of the CLI
	cmd.Env = append(os.Environ(), userRuntimeDirEnv())
	return cmd.Run()
}

// isSlotFSMounted tells whether a mirror is mounted on the folder.
func isSlotFSMounted(mountpoint string) bool {
	return slices.Contains(slotFSMounts(), mountpoint)
}

// slotFSMounts lists the mount points of the mirrors, from mountinfo.
func slotFSMounts() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseSlotFSMounts(bufio.NewScanner(f))
}

func parseSlotFSMounts(lines *bufio.Scanner) []string {
	var mounts []string
	for lines.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		before, after, ok := strings.Cut(lines.Text(), " - ")
		fields := strings.Fields(before)
		if !ok || len(fields) < 5 || !strings.HasPrefix(after, slotFSType+" ") {
			continue
		}
		mounts = append(mounts, unescapeMountinfo(fields[4]))
	}
	return mounts
}

// unescapeMountinfo decodes the octal escapes mountinfo uses for spaces, tabs, newlines and backslashes.
func unescapeMountinfo(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if v, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

// isSlotFSProcess guards a recorded pid against reuse: it must still be a mirror.
func isSlotFSProcess(pid int) bool {
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return err == nil && strings.Contains(string(cmdline), "slot-fs")
}

// slotPool coordinates the spare inside one process: the daemon warms the next spare in the
// background, and a start that finds none waits for it instead of creating a second one.
type slotPool struct {
	mu         sync.Mutex
	background bool
	warming    chan struct{}
}

var pool slotPool

// RunSlotsInBackground makes the slot work that follows a start or a stop happen after the request
// returns, as the daemon wants; a CLI call does it before it exits.
func RunSlotsInBackground() {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.background = true
}

func (p *slotPool) inBackground() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.background
}

// takeSpare returns a warm spare, waiting for the one being warmed, creating one cold without.
func takeSpare(ctx context.Context, docker command.Cli, cfg config.Configuration, platform platform.Platform) (slot, bool, error) {
	for {
		for _, s := range readSlots(cfg) {
			if s.App != "" || s.Retired {
				continue
			}
			if c, err := slotContainer(ctx, docker.Client(), s); err == nil && c != nil && c.State == container.StateRunning && s.client(cfg).Ping(ctx) == nil {
				return s, true, nil
			}
			slog.Warn("removing a spare that does not answer", slog.Int("slot", s.ID))
			removeSlot(ctx, docker.Client(), cfg, s)
		}
		pool.mu.Lock()
		warming := pool.warming
		pool.mu.Unlock()
		if warming == nil {
			s, err := createSlot(ctx, docker, cfg, platform)
			return s, false, err
		}
		select {
		case <-warming:
		case <-ctx.Done():
			return slot{}, false, ctx.Err()
		}
	}
}

// afterSlotChange removes the retired slots and warms a spare when none is left, in the background
// in the daemon, before returning otherwise.
func afterSlotChange(docker command.Cli, cfg config.Configuration, platform platform.Platform) {
	work := func() {
		ctx := context.Background()
		for _, s := range readSlots(cfg) {
			if s.Retired {
				removeSlot(ctx, docker.Client(), cfg, s)
			}
		}
		ensureSpare(ctx, docker, cfg, platform)
	}
	if pool.inBackground() {
		go work()
		return
	}
	work()
}

// ensureSpare warms a spare unless one exists or is being warmed.
func ensureSpare(ctx context.Context, docker command.Cli, cfg config.Configuration, platform platform.Platform) {
	pool.mu.Lock()
	if pool.warming != nil || slices.ContainsFunc(readSlots(cfg), func(s slot) bool { return s.App == "" && !s.Retired }) {
		pool.mu.Unlock()
		return
	}
	done := make(chan struct{})
	pool.warming = done
	pool.mu.Unlock()
	defer func() {
		pool.mu.Lock()
		pool.warming = nil
		pool.mu.Unlock()
		close(done)
	}()
	if _, err := createSlot(ctx, docker, cfg, platform); err != nil {
		slog.Warn("failed to warm a spare, the next start creates one", slog.String("error", err.Error()))
	}
}

// retireSlot stops the app of the slot and marks it for removal.
func retireSlot(ctx context.Context, cfg config.Configuration, s slot) error {
	resp, err := s.client(cfg).WithTimeout(launcherStopTimeout).Stop(ctx, "")
	var launcherErr *launcher.Error
	switch {
	case errors.As(err, &launcherErr):
		return fmt.Errorf("failed to stop the app: %w", err)
	case err != nil:
		slog.Debug("slot launcher unreachable, nothing to stop", slog.Int("slot", s.ID), slog.String("error", err.Error()))
	default:
		for _, st := range resp.Stopped {
			slog.Debug("app stopped", slog.Int("slot", s.ID), slog.String("app", s.App), slog.Int("pid", st.PID), slog.Any("exit_code", st.ExitCode), slog.Any("signal", st.Signal))
		}
	}
	s.Retired = true
	return writeSlot(cfg, s)
}

// clearSlots removes every slot, the mirrors left by a crash included, and the launcher container
// of the single-launcher layout.
func clearSlots(ctx context.Context, docker command.Cli, cfg config.Configuration) {
	for _, s := range readSlots(cfg) {
		removeSlot(ctx, docker.Client(), cfg, s)
	}
	if containers, err := dockerhelper.Containers(ctx, docker.Client(), DockerLauncherLabel+"=true"); err == nil {
		for _, c := range containers {
			_, _ = docker.Client().ContainerRemove(ctx, c.ID, dockerClient.ContainerRemoveOptions{Force: true})
		}
	}
	if hasUserManager() {
		_ = userSystemctl(ctx, "stop", slotProjectPrefix+"*-fs.service")
	}
	prefix := slotsDir(cfg).String() + string(os.PathSeparator)
	for _, mountpoint := range slotFSMounts() {
		if strings.HasPrefix(mountpoint, prefix) {
			_ = exec.Command("fusermount3", "-uz", mountpoint).Run()
		}
	}
	if err := errors.Join(slotsDir(cfg).RemoveAll(), cfg.RuntimeDir().Join("slots").RemoveAll()); err != nil {
		slog.Warn("failed to clear the slot folders", slog.String("error", err.Error()))
	}
}

// ensureAppsNetwork creates the network the slots, the sidecars and the ingress share.
func ensureAppsNetwork(ctx context.Context, docker dockerClient.APIClient) error {
	if _, err := docker.NetworkInspect(ctx, launcherNetwork, dockerClient.NetworkInspectOptions{}); err == nil {
		return nil
	}
	if _, err := docker.NetworkCreate(ctx, launcherNetwork, dockerClient.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		return fmt.Errorf("failed to create the %s network: %w", launcherNetwork, err)
	}
	return nil
}
