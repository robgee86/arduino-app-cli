// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/arduino/go-paths-helper"
)

// stateDir is the folder the provisioning writes, the .cache of an app or the prebuild dir of a
// release, with every operation bound to the folder holding it. The app writes under its own
// folder too, so a link it plants where the daemon writes must lead nowhere else: a path escaping
// the parent fails instead of being followed.
type stateDir struct {
	root *os.Root
	name string
}

// openStateDir opens the state folder, creating it when missing.
func openStateDir(genPath *paths.Path) (*stateDir, error) {
	root, err := os.OpenRoot(genPath.Parent().String())
	if err != nil {
		return nil, fmt.Errorf("cannot open the folder of %s: %w", genPath, err)
	}
	if err := root.MkdirAll(genPath.Base(), 0o755); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("cannot create %s: %w", genPath, err)
	}
	return &stateDir{root: root, name: genPath.Base()}, nil
}

func (d *stateDir) Close() error {
	return d.root.Close()
}

// WriteFile writes the file at the relative path, creating the folders leading to it.
func (d *stateDir) WriteFile(rel string, data []byte) error {
	name := path.Join(d.name, rel)
	if err := d.root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return fmt.Errorf("cannot create the folder of %s: %w", name, err)
	}
	return d.root.WriteFile(name, data, 0o644)
}

// Remove removes the file at the relative path, a missing one included.
func (d *stateDir) Remove(rel string) error {
	if err := d.root.Remove(path.Join(d.name, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveAll removes the tree at the relative path, a missing one included.
func (d *stateDir) RemoveAll(rel string) error {
	return d.root.RemoveAll(path.Join(d.name, rel))
}
