// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package pipewire

import (
	"context"
	"fmt"
	"os/user"
	"strings"
	"sync"

	"github.com/arduino/go-paths-helper"
)

var lingering struct {
	once sync.Once
	err  error
}

// EnsureLingering keeps the user's systemd --user manager running without a login session, so that
// the units started on it outlive the sessions; once per process, never undone.
func EnsureLingering(ctx context.Context) error {
	lingering.once.Do(func() { lingering.err = enableLingering(ctx) })
	return lingering.err
}

// enableLingering enables lingering for the current user if not already enabled.
func enableLingering(ctx context.Context) error {
	user, err := user.Current()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}

	alreadyOn, err := runShowLingerCommand(ctx, user.Username)
	if err != nil {
		return fmt.Errorf("check linger: %w", err)
	}
	if alreadyOn {
		return nil
	}

	return runEnableLingerCommand(ctx, user.Username)
}

func runEnableLingerCommand(ctx context.Context, username string) error {
	cmdArgs := []string{"loginctl", "enable-linger", username}
	cmd, err := paths.NewProcess(nil, cmdArgs...)
	if err != nil {
		return fmt.Errorf("create process %q: %w", strings.Join(cmdArgs, " "), err)
	}

	if out, err := cmd.RunAndCaptureCombinedOutput(ctx); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(cmdArgs, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runShowLingerCommand(ctx context.Context, username string) (bool, error) {
	cmdArgs := []string{"loginctl", "show-user", username, "-p", "Linger", "--value"}
	cmd, err := paths.NewProcess(nil, cmdArgs...)
	if err != nil {
		return false, fmt.Errorf("create process %q: %w", strings.Join(cmdArgs, " "), err)
	}

	out, err := cmd.RunAndCaptureCombinedOutput(ctx)
	if err != nil {
		if strings.Contains(string(out), "is not logged in or lingering") {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w: %s", strings.Join(cmdArgs, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}
