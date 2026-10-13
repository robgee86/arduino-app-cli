// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arduino/arduino-app-cli/cmd/arduino-app-cli/internal/servicelocator"
	"github.com/arduino/arduino-app-cli/cmd/feedback"
	"github.com/arduino/arduino-app-cli/internal/orchestrator"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

func newLauncherCmd(cfg config.Configuration) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launcher",
		Short: "Inspect and manage the app launcher container",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show what the app launcher runs",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			status, err := orchestrator.LauncherStatus(cmd.Context(), cfg)
			if err != nil {
				feedback.Fatal(err.Error(), feedback.ErrGeneric)
				return
			}
			feedback.PrintResult(launcherStatusResult(status))
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Create or update the app launcher and wait until it answers",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			elapsed, err := orchestrator.EnsureLauncher(cmd.Context(), servicelocator.GetDockerClient(), servicelocator.GetBricksIndex(), cfg, servicelocator.GetPlatform(), false)
			if err != nil {
				feedback.Fatal(err.Error(), feedback.ErrGeneric)
				return
			}
			feedback.PrintResult(launcherUpResult{Elapsed: elapsed})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Remove the app launcher, stopping every app",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out, _, getResult := feedback.OutputStreams()
			if err := orchestrator.LauncherDown(cmd.Context(), servicelocator.GetDockerClient(), cfg, func(message orchestrator.StreamMessage) {
				if message.GetType() == orchestrator.InfoType {
					fmt.Fprintln(out, "[INFO]", message.GetData())
				}
			}); err != nil {
				feedback.Fatal(err.Error(), feedback.ErrGeneric)
				return
			}
			feedback.PrintResult(launcherDownResult{Output: getResult()})
		},
	})
	return cmd
}

type launcherStatusResult launcher.StatusResponse

func (r launcherStatusResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "App launcher %s, up %s\n", r.Version, (time.Duration(r.UptimeS) * time.Second).String())
	if len(r.Children) == 0 {
		b.WriteString("No app running")
	}
	for _, c := range r.Children {
		fmt.Fprintf(&b, "%s: %s, pid %d, port %d, warm %t, since %s\n", c.App, c.State, c.PID, c.HTTPPort, c.Warm, c.StartedAt.Local().Format(time.DateTime))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (r launcherStatusResult) Data() any {
	return launcher.StatusResponse(r)
}

type launcherUpResult struct {
	Elapsed time.Duration `json:"elapsed"`
}

func (r launcherUpResult) String() string {
	return fmt.Sprintf("✓ App launcher ready in %s", r.Elapsed.Round(time.Millisecond))
}

func (r launcherUpResult) Data() any {
	return r
}

type launcherDownResult struct {
	Output *feedback.OutputStreamsResult `json:"output,omitempty"`
}

func (r launcherDownResult) String() string {
	return "✓ App launcher removed"
}

func (r launcherDownResult) Data() any {
	return r
}
