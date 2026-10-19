// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package fatomic

import (
	"fmt"
	"os"
	"path"
)

// WriteFileIn replaces name within root atomically, as WriteFile does, and syncs the file and its
// directory. Every step stays within the root: a link leading out of it fails the write.
func WriteFileIn(root *os.Root, name string, data []byte, perm os.FileMode) error {
	tmp := name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err := cleanClose(f, err); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	dir, err := root.Open(path.Dir(name))
	if err != nil {
		return err
	}
	return cleanClose(dir, dir.Sync())
}

func cleanClose(f *os.File, err error) error {
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
