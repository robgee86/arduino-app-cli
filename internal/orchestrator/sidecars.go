// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// The sidecars zone is the one compose project of the board sidecars. Each start writes it from
// the sidecars of the app being started, and compose up reconciles the containers with it: a
// service whose configuration is unchanged keeps running, a changed one is recreated, the services
// no longer in it are removed. The zone runs the sidecars of the wanted app, see launcherWantedAppFile.
const sidecarsProject = "arduino-sidecars"

func sidecarsDir(cfg config.Configuration) *paths.Path {
	return cfg.DataDir().Join("sidecars")
}

func sidecarsComposeFile(cfg config.Configuration) *paths.Path {
	return sidecarsDir(cfg).Join("compose.yaml")
}

// sidecarsFingerprintFile holds the fingerprint of the zone the containers were last brought to.
func sidecarsFingerprintFile(cfg config.Configuration) *paths.Path {
	return sidecarsDir(cfg).Join("fingerprint")
}

// sidecarsZone is a generated zone: the project of its services, none without sidecars, and the
// fingerprint of the project.
type sidecarsZone struct {
	project     *types.Project
	services    []string
	fingerprint string
}

// generateSidecarsZone turns the rendered app project, its main service split off, into the zone:
// on the network shared with the launcher, and naming no app so that two apps with the same
// sidecars get the same containers. It writes the zone compose file, and removes it without sidecars.
func generateSidecarsZone(prj *types.Project, cfg config.Configuration) (sidecarsZone, error) {
	slog.Debug("Generating the sidecars zone", slog.String("app", prj.Name))
	if len(prj.Services) == 0 {
		slog.Debug("No sidecars to run, removing the sidecars zone files")
		return sidecarsZone{}, removeSidecarsZoneFiles(cfg)
	}
	if err := sidecarsDir(cfg).MkdirAll(); err != nil {
		return sidecarsZone{}, fmt.Errorf("failed to create the sidecars folder: %w", err)
	}

	zone := prj.WithoutUnnecessaryResources()
	zone.Name = sidecarsProject
	// The launcher reaches the sidecars by service name
	zone.Networks = types.Networks{"default": types.NetworkConfig{Name: launcherNetwork, External: true}}
	for name, svc := range zone.Services {
		// The app folder reaches a sidecar only through its compose file, as ${APP_HOME}
		delete(svc.Labels, DockerAppPathLabel)
		delete(svc.Environment, "APP_HOME")
		zone.Services[name] = svc
	}
	// A named volume is the zone's, as its services are, unless a compose file names it
	for key, vol := range zone.Volumes {
		if !vol.External && vol.Name == prj.Name+"_"+key {
			vol.Name = sidecarsProject + "_" + key
			zone.Volumes[key] = vol
		}
	}

	data, err := zone.MarshalYAML()
	if err != nil {
		return sidecarsZone{}, err
	}
	// compose-go escapes nothing it writes: a value holding a $ would be substituted again
	data = bytes.ReplaceAll(data, []byte("$"), []byte("$$"))
	if err := fatomic.WriteFile(sidecarsComposeFile(cfg).String(), data, 0o644); err != nil {
		return sidecarsZone{}, fmt.Errorf("failed to write the sidecars compose file: %w", err)
	}
	zone.ComposeFiles = []string{sidecarsComposeFile(cfg).String()}
	zone.WorkingDir = sidecarsDir(cfg).String()

	sum := sha256.Sum256(data)
	return sidecarsZone{project: zone, services: zone.ServiceNames(), fingerprint: hex.EncodeToString(sum[:])}, nil
}

func readSidecarsFingerprint(cfg config.Configuration) string {
	data, _ := sidecarsFingerprintFile(cfg).ReadFile()
	return string(data)
}

// writeSidecarsFingerprint records the zone the containers run, or forgets it when empty.
func writeSidecarsFingerprint(cfg config.Configuration, fingerprint string) error {
	file := sidecarsFingerprintFile(cfg)
	if fingerprint == "" {
		if err := file.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove the sidecars fingerprint: %w", err)
		}
		return nil
	}
	return fatomic.WriteFile(file.String(), []byte(fingerprint), 0o644)
}

func removeSidecarsZoneFiles(cfg config.Configuration) error {
	for _, file := range []*paths.Path{sidecarsComposeFile(cfg), sidecarsFingerprintFile(cfg)} {
		if err := file.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove %s: %w", file, err)
		}
	}
	return nil
}

// splitAppContainers separates the zone containers from the ones of the per-app projects of the older versions.
func splitAppContainers(containers []container.Summary) (zone appContainers, legacy []container.Summary) {
	zone = make(appContainers)
	for _, c := range containers {
		if c.Labels[composeProjectLabel] == sidecarsProject {
			zone[c.Labels[composeServiceLabel]] = c
		} else {
			legacy = append(legacy, c)
		}
	}
	return zone, legacy
}

// reconcileSidecars brings the zone containers to the zone and returns once they run. An unchanged
// zone whose containers exist is left running, or started through the SDK, faster than compose.
func reconcileSidecars(ctx context.Context, docker command.Cli, cfg config.Configuration, zone sidecarsZone, cb func(StreamMessage)) error {
	all, err := listAppContainers(ctx, docker.Client())
	if err != nil {
		return err
	}
	containers, legacy := splitAppContainers(all)
	if len(legacy) > 0 {
		slog.Info("removing the sidecars of the per-app projects", slog.Int("containers", len(legacy)))
		if err := removeContainers(ctx, docker.Client(), legacy); err != nil {
			return err
		}
	}

	if len(zone.services) == 0 {
		return errors.Join(writeSidecarsFingerprint(cfg, ""), removeContainers(ctx, docker.Client(), slices.Collect(maps.Values(containers))))
	}
	if zone.fingerprint == "" || zone.fingerprint != readSidecarsFingerprint(cfg) || !containers.cover(zone.services) {
		return upSidecars(ctx, docker, cfg, zone, cb)
	}
	stopped := containers.notRunning()
	if len(stopped) == 0 {
		slog.Debug("sidecars zone unchanged and running")
		return nil
	}
	slog.Debug("starting the existing sidecar containers")
	err = startAppContainers(ctx, docker.Client(), stopped, cb)
	if err == nil {
		return nil
	}
	// e.g. the shared network was recreated with the launcher
	slog.Warn("failed to start the existing sidecars, recreating them", slog.String("error", err.Error()))
	if err := removeContainers(ctx, docker.Client(), slices.Collect(maps.Values(containers))); err != nil {
		return err
	}
	return upSidecars(ctx, docker, cfg, zone, cb)
}

// upSidecars runs compose up on the zone, which removes the orphans before it creates or recreates
// a service, so that a new service can take the ports of a removed one. It returns once they run.
func upSidecars(ctx context.Context, docker command.Cli, cfg config.Configuration, zone sidecarsZone, cb func(StreamMessage)) error {
	// A failed up leaves the containers half way: the next start runs it again
	if err := writeSidecarsFingerprint(cfg, ""); err != nil {
		return err
	}
	slog.Debug("starting the sidecars", slog.Any("services", zone.services))
	if err := dockerhelper.ComposeUp(ctx, docker, zone.project, func(line string) { cb(StreamMessage{data: line}) }); err != nil {
		return err
	}
	// up returns once it started the containers: this catches one that exited right away
	created, err := getAppContainers(ctx, docker.Client(), sidecarsProject)
	if err != nil {
		return err
	}
	if err := waitContainersRunning(ctx, docker.Client(), created); err != nil {
		return err
	}
	return writeSidecarsFingerprint(cfg, zone.fingerprint)
}

// stopSidecars stops the zone containers, which stay for the next start.
func stopSidecars(ctx context.Context, docker dockerClient.APIClient, cb func(StreamMessage)) error {
	containers, err := getAppContainers(ctx, docker, sidecarsProject)
	if err != nil {
		return err
	}
	return stopAppContainers(ctx, docker, containers, DefaultDockerStopTimeoutSeconds, cb)
}

// removeSidecars removes the zone containers: the next start creates them again.
func removeSidecars(ctx context.Context, docker dockerClient.APIClient, cfg config.Configuration) error {
	containers, err := getAppContainers(ctx, docker, sidecarsProject)
	if err != nil {
		return err
	}
	return errors.Join(writeSidecarsFingerprint(cfg, ""), removeContainers(ctx, docker, slices.Collect(maps.Values(containers))))
}
