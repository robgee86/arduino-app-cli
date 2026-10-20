// SPDX-FileCopyrightText: Copyright (C) Arduino s.r.l. and/or its affiliated companies
//
// SPDX-License-Identifier: MPL-2.0

package orchestrator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

// The app shares its uid and the socket folder with its supervisor: it can replace the socket with a link to
// another app's or with a listener of its own. The daemon therefore trusts no path and no reply by itself: a
// connection is accepted only from the direct child of the container init, the supervisor, and a stop the
// supervisor does not confirm, or does not complete, ends with a restart of the container.

// procRoot is where the process table is read, replaced in tests.
var procRoot = "/proc"

// initPIDs caches the host pid of each app container's init by container name, dropped on a restart.
var initPIDs sync.Map

// restartGraceSeconds is how long a restart waits before SIGKILL: the app is being removed, not asked.
const restartGraceSeconds = 2

// client talks to the supervisor of the container and to nothing else.
func (c appContainer) client(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration) *launcher.Client {
	socket := c.runtimeDir(cfg).Join(launcher.SocketName).String()
	return launcher.New(socket, launcherRequestTimeout).WithPeer(c.supervisorCheck(ctx, docker))
}

// supervisorCheck accepts the direct child of the container init only: not the app, not a listener it planted
// on the socket path, not the supervisor of another container reached through a link.
func (c appContainer) supervisorCheck(ctx context.Context, docker dockerClient.APIClient) launcher.PeerCheck {
	return func(pid int) error {
		parent, err := parentPID(pid)
		if err != nil {
			return err
		}
		// A restarted container has a new init: the cache is refreshed once before refusing
		for _, refresh := range []bool{false, true} {
			init, err := c.initPID(ctx, docker, refresh)
			if err != nil {
				return err
			}
			if parent == init {
				return nil
			}
		}
		return fmt.Errorf("pid %d is a child of %d, not of the init of %s", pid, parent, c.containerName())
	}
}

// initPID is the host pid of the container init, one inspect per container lifetime.
func (c appContainer) initPID(ctx context.Context, docker dockerClient.APIClient, refresh bool) (int, error) {
	name := c.containerName()
	if !refresh {
		if pid, ok := initPIDs.Load(name); ok {
			return pid.(int), nil
		}
	}
	inspected, err := docker.ContainerInspect(ctx, name, dockerClient.ContainerInspectOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to inspect the app container: %w", err)
	}
	if inspected.Container.State == nil || inspected.Container.State.Pid == 0 {
		return 0, fmt.Errorf("the app container %s is not running", name)
	}
	initPIDs.Store(name, inspected.Container.State.Pid)
	return inspected.Container.State.Pid, nil
}

// leftovers tells whether the container holds any process beyond its init and the supervisor: what a stop
// must have taken away. The kernel of the UNO Q lists no children files, so the process table is scanned.
func (c appContainer) leftovers(ctx context.Context, docker dockerClient.APIClient) (bool, error) {
	init, err := c.initPID(ctx, docker, false)
	if err != nil {
		return false, err
	}
	tree, err := processTree()
	if err != nil {
		return false, err
	}
	return len(descendants(tree, init)) > 1, nil
}

// restartAppContainer restarts the container when its supervisor cannot be trusted or did not stop the app:
// the pid namespace dies with it and the container comes back warm. A missing container needs nothing.
func restartAppContainer(ctx context.Context, docker dockerClient.APIClient, c appContainer, reason string) error {
	slog.Warn("restarting the app container", slog.String("project", c.project), slog.String("reason", reason))
	initPIDs.Delete(c.containerName())
	timeout := restartGraceSeconds
	_, err := docker.ContainerRestart(ctx, c.containerName(), dockerClient.ContainerRestartOptions{Timeout: &timeout})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to restart the app container: %w", err)
	}
	return nil
}

// parentPID is the parent of the process, from its status file.
func parentPID(pid int) (int, error) {
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, fmt.Errorf("the peer is gone: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "PPid:"); ok {
			return strconv.Atoi(strings.TrimSpace(value))
		}
	}
	return 0, errors.New("the status of the peer names no parent")
}

// processTree maps every process to its parent.
func processTree() (map[int]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	tree := make(map[int]int, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// A process gone between the listing and the read is no leftover
		if parent, err := parentPID(pid); err == nil {
			tree[pid] = parent
		}
	}
	return tree, nil
}

// descendants lists the processes below root, root excluded.
func descendants(tree map[int]int, root int) []int {
	children := make(map[int][]int, len(tree))
	for pid, parent := range tree {
		children[parent] = append(children[parent], pid)
	}
	var found []int
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			found = append(found, child)
			queue = append(queue, child)
		}
	}
	return found
}
