// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/arduino/go-paths-helper"
)

const appLogPollInterval = 250 * time.Millisecond

// appLogFile is where the launcher writes the app output, within the app folder, rotated into app.log.1.
const appLogFile = ".cache/logs/app.log"

// readAppLogFile yields the last tail lines of the app log, all of them when tail is nil, then
// with follow the lines appended later, across rotations, until ctx is done. The log lies where
// the app may plant a link: it is read within the app folder, through which no link leads out.
func readAppLogFile(ctx context.Context, appDir *paths.Path, tail *uint64, follow bool, yield func(LogMessage) bool) {
	emit := func(line []byte) bool {
		return yield(LogMessage{Source: LogSourceMain, ContainerName: mainServiceName, Content: string(line)})
	}

	root, err := os.OpenRoot(appDir.String())
	if err != nil {
		slog.Debug("cannot open the app folder", slog.String("path", appDir.String()), slog.String("error", err.Error()))
		return
	}
	defer root.Close()

	follower := &logFollower{root: root, path: appLogFile}
	defer follower.close()
	follower.open()
	follower.read()
	rotated := readWithin(root, appLogFile+".1")
	data := slices.Concat(rotated, follower.pending)
	follower.pending = nil
	if follow {
		// A partial last line is completed by the next writes
		data, follower.pending = cutPartialLine(data)
	}

	lines := splitLines(data)
	if tail != nil && uint64(len(lines)) > *tail {
		lines = lines[uint64(len(lines))-*tail:]
	}
	for _, line := range lines {
		if !emit(line) {
			return
		}
	}
	if !follow {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(appLogPollInterval):
		}
		for _, line := range follower.poll() {
			if !emit(line) {
				return
			}
		}
	}
}

// readWithin returns the content of the file at path within the root, nothing when it cannot be read.
func readWithin(root *os.Root, path string) []byte {
	file, err := root.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, _ := io.ReadAll(file)
	return data
}

// logFollower reads what is appended to a log, reopening it when it is rotated or truncated.
type logFollower struct {
	root    *os.Root
	path    string
	file    *os.File
	pending []byte
}

// poll returns the complete lines written since the last call.
func (f *logFollower) poll() [][]byte {
	if f.file == nil {
		f.open()
	}
	f.read()
	// The rest of the old file was read above: continue from the start of the new one
	if f.replaced() {
		f.close()
		f.open()
		f.read()
	}
	var complete []byte
	complete, f.pending = cutPartialLine(f.pending)
	return splitLines(complete)
}

func (f *logFollower) open() {
	file, err := f.root.Open(f.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Debug("cannot open the app log", slog.String("path", f.path), slog.String("error", err.Error()))
		}
		return
	}
	f.file = file
}

func (f *logFollower) read() {
	if f.file == nil {
		return
	}
	data, err := io.ReadAll(f.file)
	if err != nil {
		slog.Debug("cannot read the app log", slog.String("path", f.path), slog.String("error", err.Error()))
	}
	f.pending = append(f.pending, data...)
}

// replaced tells whether the path is now another file, or the file got shorter than what was read.
func (f *logFollower) replaced() bool {
	if f.file == nil {
		return false
	}
	onPath, err := f.root.Stat(f.path)
	if err != nil {
		return false
	}
	opened, err := f.file.Stat()
	if err != nil {
		return false
	}
	offset, err := f.file.Seek(0, io.SeekCurrent)
	return !os.SameFile(onPath, opened) || (err == nil && opened.Size() < offset)
}

func (f *logFollower) close() {
	if f.file != nil {
		_ = f.file.Close()
		f.file = nil
	}
}

// cutPartialLine splits data after its last newline.
func cutPartialLine(data []byte) (complete, partial []byte) {
	end := bytes.LastIndexByte(data, '\n') + 1
	return data[:end], bytes.Clone(data[end:])
}

// splitLines splits on newlines, keeping a last line without one.
func splitLines(data []byte) [][]byte {
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) == 0 {
		return nil
	}
	return bytes.Split(data, []byte("\n"))
}
