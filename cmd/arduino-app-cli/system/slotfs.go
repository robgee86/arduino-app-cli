// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package system

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/arduino/arduino-app-cli/internal/slotfs"
)

// newSlotFSCmd serves the app folder mirror of a slot. The orchestrator starts it, never a user.
func newSlotFSCmd() *cobra.Command {
	var cacheTimeout time.Duration
	cmd := &cobra.Command{
		Use:    "slot-fs <app folder> <mount point>",
		Args:   cobra.ExactArgs(2),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return slotfs.Serve(args[0], args[1], cacheTimeout)
		},
	}
	cmd.Flags().DurationVar(&cacheTimeout, "cache-timeout", time.Second, "how long the kernel trusts the names and attributes it read")
	return cmd
}
