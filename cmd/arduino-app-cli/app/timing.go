// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arduino/arduino-app-cli/cmd/arduino-app-cli/internal/servicelocator"
	"github.com/arduino/arduino-app-cli/internal/orchestrator"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

// newTimingCmd runs orchestrator steps one after the other in this process and prints when
// each began and returned: a benchmark tool, free of the time a new process takes to load.
func newTimingCmd(cfg config.Configuration) *cobra.Command {
	return &cobra.Command{
		Use:    "timing op:app_path...",
		Short:  "Time start, stop, down, reload and keep steps on apps",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			docker := servicelocator.GetDockerClient()
			provisioner := servicelocator.GetProvisioner()
			models, bricks, services := servicelocator.GetModelsIndex(), servicelocator.GetBricksIndex(), servicelocator.GetServicesIndex()
			platform := servicelocator.GetPlatform()

			for _, step := range args {
				op, path, _ := strings.Cut(step, ":")
				target, err := Load(path)
				if err != nil {
					return err
				}

				began := time.Now()
				detail := ""
				switch op {
				case "start":
					err = orchestrator.StartApp(ctx, docker, provisioner, models, bricks, services, target, cfg, platform, false, true, nil)
				case "stop":
					err = orchestrator.StopApp(ctx, docker, platform, target, cfg, nil)
				case "down":
					err = orchestrator.StopAndDestroyApp(ctx, docker, platform, target, cfg, nil)
				case "reload":
					err = orchestrator.ReloadApp(ctx, docker, provisioner, models, bricks, services, target, cfg, platform, nil)
				case "keep":
					var timings launcher.Timings
					timings, err = orchestrator.StopAppKeepingContainer(ctx, docker, target, cfg)
					detail = fmt.Sprintf(" launcher_stop_ms=%.0f", timings.StopMs)
				default:
					return fmt.Errorf("unknown step %q", op)
				}
				fmt.Fprintf(os.Stderr, "TIMING %s %s began=%d returned=%d took=%d%s err=%v\n",
					op, target.FullPath.Base(), began.UnixMilli(), time.Now().UnixMilli(), time.Since(began).Milliseconds(), detail, err)
				if err != nil {
					return nil
				}
			}
			return nil
		},
	}
}
