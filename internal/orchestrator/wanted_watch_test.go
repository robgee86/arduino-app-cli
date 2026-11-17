// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

// fakeSupervisors stands in for the supervisors of the app containers: each call of the events
// of an app plays the next of its scripted connections, then holds until it is dropped.
type fakeSupervisors struct {
	mu          sync.Mutex
	connections map[string][]func(onState func(launcher.AppState)) error
	asked       map[string]int
	dropped     chan string
}

func newFakeSupervisors() *fakeSupervisors {
	return &fakeSupervisors{connections: map[string][]func(func(launcher.AppState)) error{}, asked: map[string]int{}, dropped: make(chan string, 8)}
}

func (f *fakeSupervisors) events(ctx context.Context, appPath *paths.Path, onState func(launcher.AppState)) error {
	app := appPath.String()
	f.mu.Lock()
	n := f.asked[app]
	f.asked[app]++
	connections := f.connections[app]
	f.mu.Unlock()
	if n < len(connections) {
		if err := connections[n](onState); err != nil {
			return err
		}
	}
	<-ctx.Done()
	f.dropped <- app
	return ctx.Err()
}

func (f *fakeSupervisors) timesAsked(app string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[app]
}

func (f *fakeSupervisors) watch() *wantedWatch {
	return &wantedWatch{events: f.events, retry: time.Millisecond, updates: make(chan wantedUpdate)}
}

func nextWantedUpdate(t *testing.T, watch *wantedWatch) wantedUpdate {
	t.Helper()
	select {
	case update := <-watch.updates:
		return update
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the supervisor told nothing")
		return wantedUpdate{}
	}
}

func TestTheSupervisorOfTheWantedAppTellsWhenItsChildStartsOrEnds(t *testing.T) {
	code := 1
	fake := newFakeSupervisors()
	fake.connections["/apps/one"] = []func(func(launcher.AppState)) error{func(onState func(launcher.AppState)) error {
		onState(launcher.AppState{})
		onState(launcher.AppState{Child: &launcher.Child{PID: 42}})
		onState(launcher.AppState{Last: &launcher.Ended{PID: 42, ExitCode: &code}})
		return nil
	}}
	watch := fake.watch()
	one := paths.New("/apps/one")

	_, known := watch.state(t.Context(), one)
	assert.False(t, known, "until the supervisor tells, it is asked")

	require.True(t, watch.apply(nextWantedUpdate(t, watch)))
	state, known := watch.state(t.Context(), one)
	assert.True(t, known)
	assert.Equal(t, launcher.AppState{}, state)

	require.True(t, watch.apply(nextWantedUpdate(t, watch)))
	state, _ = watch.state(t.Context(), one)
	require.NotNil(t, state.Child)
	assert.Equal(t, 42, state.Child.PID)

	require.True(t, watch.apply(nextWantedUpdate(t, watch)))
	state, known = watch.state(t.Context(), one)
	assert.True(t, known)
	assert.Nil(t, state.Child)
	assert.Equal(t, 1, *state.Last.ExitCode)
	assert.Equal(t, 1, fake.timesAsked("/apps/one"), "one connection tells it all")
}

func TestOnlyTheSupervisorOfTheWantedAppIsFollowed(t *testing.T) {
	fake := newFakeSupervisors()
	running := func(onState func(launcher.AppState)) error {
		onState(launcher.AppState{Child: &launcher.Child{}})
		return nil
	}
	fake.connections["/apps/one"] = []func(func(launcher.AppState)) error{running}
	fake.connections["/apps/two"] = []func(func(launcher.AppState)) error{running}
	watch := fake.watch()

	watch.state(t.Context(), paths.New("/apps/one"))
	update := nextWantedUpdate(t, watch)
	require.True(t, watch.apply(update))

	// Another app is wanted: the supervisor of the first is dropped, what it told is forgotten
	_, known := watch.state(t.Context(), paths.New("/apps/two"))
	assert.False(t, known)
	assert.Equal(t, "/apps/one", <-fake.dropped)
	assert.False(t, watch.apply(update), "what the first told before is not taken for a word of the second")
	require.True(t, watch.apply(nextWantedUpdate(t, watch)))
	_, known = watch.state(t.Context(), paths.New("/apps/two"))
	assert.True(t, known)

	// No app is wanted: no supervisor is followed
	_, known = watch.state(t.Context(), nil)
	assert.False(t, known)
	assert.Equal(t, "/apps/two", <-fake.dropped)
}

func TestASupervisorIsAskedAgainUntilItGivesItsEvents(t *testing.T) {
	notUp := func(func(launcher.AppState)) error { return errors.New("cannot reach the app launcher") }
	fake := newFakeSupervisors()
	fake.connections["/apps/fresh"] = []func(func(launcher.AppState)) error{notUp, notUp, func(onState func(launcher.AppState)) error {
		onState(launcher.AppState{Child: &launcher.Child{PID: 42}})
		return errors.New("connection closed")
	}, func(onState func(launcher.AppState)) error {
		onState(launcher.AppState{Child: &launcher.Child{PID: 43}})
		return nil
	}}
	watch := fake.watch()
	fresh := paths.New("/apps/fresh")
	watch.state(t.Context(), fresh)

	require.True(t, watch.apply(nextWantedUpdate(t, watch)))
	state, known := watch.state(t.Context(), fresh)
	assert.True(t, known)
	assert.Equal(t, 42, state.Child.PID)

	// Its events ended, as when it becomes the venv interpreter of its app: nothing changed, it is asked until it tells again
	ended := nextWantedUpdate(t, watch)
	assert.True(t, ended.ended)
	assert.False(t, watch.apply(ended))
	_, known = watch.state(t.Context(), fresh)
	assert.False(t, known)

	require.True(t, watch.apply(nextWantedUpdate(t, watch)))
	state, known = watch.state(t.Context(), fresh)
	assert.True(t, known)
	assert.Equal(t, 43, state.Child.PID)
	assert.Equal(t, 4, fake.timesAsked("/apps/fresh"))
}

func TestASupervisorWithoutEventsIsNotAskedForThemAgain(t *testing.T) {
	fake := newFakeSupervisors()
	fake.connections["/apps/old"] = []func(func(launcher.AppState)) error{func(func(launcher.AppState)) error {
		return &launcher.Error{Code: launcher.CodeBadRequest, Message: "unknown cmd: 'events'"}
	}}
	watch := fake.watch()
	old := paths.New("/apps/old")
	watch.state(t.Context(), old)

	assert.Never(t, func() bool { return fake.timesAsked("/apps/old") > 1 }, 50*time.Millisecond, time.Millisecond)
	_, known := watch.state(t.Context(), old)
	assert.False(t, known, "it is asked about its app at each tick, as before")
}
