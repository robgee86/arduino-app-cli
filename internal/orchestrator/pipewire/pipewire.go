// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Package pipewire brings up the PipeWire audio stack for the current
// process's own user. user@<uid>.service's own lifecycle is left entirely to
// logind, driven by linger (see enableLingering in linger.go)

package pipewire

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/arduino/go-paths-helper"
)

const (
	userManagerDialAttempts = 30
	userManagerDialInterval = 500 * time.Millisecond
)

var ensured sync.Once

// EnsurePipewireRunning starts the PipeWire units (pipewire, pipewire-pulse and
// wireplumber) for the current user. They need the user's systemd --user manager:
// when it is not reachable, lingering is enabled so that logind brings it up and
// keeps it running. Lingering is never disabled again: the audio stack stays up
// across app starts and stops, so only the first call of a process does any work.
func EnsurePipewireRunning(ctx context.Context) (err error) {
	ensured.Do(func() { err = ensurePipewireRunning(ctx) })
	return err
}

func ensurePipewireRunning(ctx context.Context) error {
	// A user manager provided by a session or by a systemd dependency is enough
	if err := runStartPipewireUnitsCommand(ctx); err == nil {
		return nil
	}

	if err := enableLingering(ctx); err != nil {
		return fmt.Errorf("failed to enable audio service linger: %w", err)
	}

	var lastErr error
	for range userManagerDialAttempts {
		if err := runStartPipewireUnitsCommand(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(userManagerDialInterval):
		}
	}
	return fmt.Errorf("wait for user systemd manager: %w", lastErr)
}

func runStartPipewireUnitsCommand(ctx context.Context) error {
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid())}
	cmd, err := paths.NewProcess(env,
		"systemctl",
		"--user",
		"start",
		"pipewire.socket",
		"pipewire-pulse.socket",
		"wireplumber.service",
	)
	if err != nil {
		return fmt.Errorf("create process start pipewire: %w", err)
	}

	if out, err := cmd.RunAndCaptureCombinedOutput(ctx); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(cmd.GetArgs(), " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
