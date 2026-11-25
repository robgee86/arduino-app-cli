// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"

	"github.com/docker/cli/cli/command"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

// StopAppKeepingContainer ends the Python code of a running app through its launcher and
// leaves its containers up: ReloadApp then starts the app again from the warm interpreter.
//
// It is the one step the proof of concept adds, and it works on the given app alone. The
// kept container still holds its ports and still reads as running, so a start of another
// app is refused until StopApp stops it.
func StopAppKeepingContainer(ctx context.Context, docker command.Cli, appToStop app.ArduinoApp, cfg config.Configuration) (launcher.Timings, error) {
	containers, err := dockerhelper.Containers(ctx, docker.Client(), DockerAppPathLabel+"="+appToStop.FullPath.String())
	if err != nil {
		return launcher.Timings{}, err
	}
	main := launcherContainer(containers)
	if main == nil {
		return launcher.Timings{}, fmt.Errorf("cannot keep the container of app %q: %w", appToStop.Name, ErrAppNotRunning)
	}
	resp, err := launcherClient(ctx, docker.Client(), cfg, appToStop.FullPath, main.ID).Stop(ctx)
	if err != nil {
		return launcher.Timings{}, fmt.Errorf("cannot stop app %q in its container: %w", appToStop.Name, err)
	}
	return resp.Timings, nil
}
