// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Package slotfs mirrors an app folder onto the folder a slot container sees as /app, through FUSE.
// It needs no privilege: fusermount3, the setuid helper of the fuse3 package, mounts it for the
// user owning the mount point, and only that user may use it.
package slotfs

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Name is the filesystem type the mirror shows as, fuse.slotfs in mountinfo.
const Name = "slotfs"

// Serve mounts the mirror of source on mountpoint and serves it until it is unmounted or the
// process is told to stop. cacheTimeout is how long the kernel trusts the names and attributes it
// read: an edit made on the host shows through the mirror after it.
func Serve(source, mountpoint string, cacheTimeout time.Duration) error {
	root, err := fs.NewLoopbackRoot(source)
	if err != nil {
		return fmt.Errorf("cannot mirror %s: %w", source, err)
	}
	// Names that do not exist are asked again every time: a file the user creates shows at once
	negative := time.Duration(0)
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		EntryTimeout:    &cacheTimeout,
		AttrTimeout:     &cacheTimeout,
		NegativeTimeout: &negative,
		// The permissions of the app files stay as they are, 000 included
		NullPermissions: true,
		MountOptions: fuse.MountOptions{
			FsName: source,
			Name:   Name,
		},
	})
	if err != nil {
		return fmt.Errorf("cannot mount the mirror of %s on %s: %w", source, mountpoint, err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		_ = server.Unmount()
	}()
	server.Wait()
	return nil
}
