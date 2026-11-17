// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/arduino/go-paths-helper"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/launcher"
)

const (
	// wantedEventsRetry is the first wait before asking again a supervisor that gave no events,
	// as while it becomes the venv interpreter of its app, and wantedEventsMaxRetry the longest.
	wantedEventsRetry    = 100 * time.Millisecond
	wantedEventsMaxRetry = time.Second
)

// appEvents follows the app of a container: it calls onState with the state of the app as it
// is now, then at each change, until the supervisor goes or the context is done.
type appEvents func(ctx context.Context, appPath *paths.Path, onState func(launcher.AppState)) error

// wantedWatch keeps the events of the supervisor of the wanted app, one app at a time, and
// what it last told. One loop owns it: the follower reaches it through updates only.
type wantedWatch struct {
	events   appEvents
	retry    time.Duration
	updates  chan wantedUpdate
	followed *followedApp
}

type followedApp struct {
	app  *paths.Path
	stop context.CancelFunc
	// state is what the supervisor last told. It is not known until the supervisor tells, once
	// its events ended, and for a supervisor without events.
	state launcher.AppState
	known bool
}

// wantedUpdate is what the followed supervisor just told: the state of its app, or that its events ended.
type wantedUpdate struct {
	from  *followedApp
	state launcher.AppState
	ended bool
}

func newWantedWatch(docker dockerClient.APIClient, cfg config.Configuration) *wantedWatch {
	events := func(ctx context.Context, appPath *paths.Path, onState func(launcher.AppState)) error {
		return appContainerOf(cfg, appPath).client(ctx, docker, cfg).Events(ctx, onState)
	}
	return &wantedWatch{events: events, retry: wantedEventsRetry, updates: make(chan wantedUpdate)}
}

// state follows the supervisor of the wanted app, dropping the one of the app wanted before,
// and returns what it last told, false while that is not known.
func (w *wantedWatch) state(ctx context.Context, wanted *paths.Path) (launcher.AppState, bool) {
	if w.followed != nil && (wanted == nil || !w.followed.app.EqualsTo(wanted)) {
		w.followed.stop()
		w.followed = nil
	}
	if wanted == nil {
		return launcher.AppState{}, false
	}
	if w.followed == nil {
		followCtx, stop := context.WithCancel(ctx)
		w.followed = &followedApp{app: wanted, stop: stop}
		go w.follow(followCtx, w.followed)
	}
	return w.followed.state, w.followed.known
}

// follow sends each state the supervisor tells, and that its events ended when they do, then
// asks again after a wait: the supervisor of a fresh container answers once it is up, and one
// that becomes the venv interpreter of its app drops its connections.
func (w *wantedWatch) follow(ctx context.Context, from *followedApp) {
	send := func(update wantedUpdate) {
		select {
		case w.updates <- update:
		case <-ctx.Done():
		}
	}
	wait := w.retry
	for {
		told := false
		err := w.events(ctx, from.app, func(state launcher.AppState) {
			told, wait = true, w.retry
			send(wantedUpdate{from: from, state: state})
		})
		if ctx.Err() != nil {
			return
		}
		if told {
			send(wantedUpdate{from: from, ended: true})
		}
		if launcher.IsCode(err, launcher.CodeBadRequest) {
			slog.Warn("the app supervisor has no events, it is asked about its app at each tick",
				slog.String("app", from.app.String()), slog.String("error", err.Error()))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, wantedEventsMaxRetry)
	}
}

// apply records what a supervisor told and reports whether the state of its app changed: not
// for a supervisor no longer followed, nor when its events ended, after which what it last
// told stands until it tells again or the next tick asks it.
func (w *wantedWatch) apply(update wantedUpdate) bool {
	if w.followed == nil || w.followed != update.from {
		return false
	}
	w.followed.known = !update.ended
	if update.ended {
		return false
	}
	w.followed.state = update.state
	return true
}

// followedAppsView is the apps view of the status events: the supervisor of the wanted app
// tells its child, and is asked for it only while it tells nothing.
func followedAppsView(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration, watch *wantedWatch) appsView {
	view := wantedView(cfg)
	state, known := watch.state(ctx, view.wanted)
	if !known {
		return askWantedApp(ctx, docker, cfg, view)
	}
	view.child, view.last = state.Child, state.Last
	return view
}
