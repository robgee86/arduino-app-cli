// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arduino/arduino-app-cli/internal/fatomic"
)

// EnvironmentFile is where an app finds the environment it runs with, relative to its folder: the
// app process reads it once confined, after the fork. The launcher process never sees it, so
// nothing of one app, its secrets included, lingers in the memory the next app is forked from.
var EnvironmentFile = filepath.Join(".cache", "environment.json")

// WriteEnvironment writes the environment of the app into its folder, readable by its uid only.
// Every step stays within the app folder: a link the app planted on the way fails the write.
func WriteEnvironment(appDir string, env map[string]string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(appDir)
	if err != nil {
		return fmt.Errorf("cannot open the app folder: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(EnvironmentFile), 0o755); err != nil {
		return fmt.Errorf("cannot create the cache folder of the app: %w", err)
	}
	if err := fatomic.WriteFileIn(root, EnvironmentFile, data, 0o600); err != nil {
		return fmt.Errorf("cannot write the app environment: %w", err)
	}
	return nil
}
