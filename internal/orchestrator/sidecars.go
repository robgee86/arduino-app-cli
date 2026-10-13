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
	"encoding/json"
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

// sidecarsRecordFile holds the fingerprint of each zone service as its container runs: the services
// it lists are the sidecars of the wanted app. It describes the containers, not the zone files.
func sidecarsRecordFile(cfg config.Configuration) *paths.Path {
	return sidecarsDir(cfg).Join("fingerprints.json")
}

// sidecarsZone is a generated zone: the project of its services, none without sidecars, and the
// fingerprint of each service as compose resolves it.
type sidecarsZone struct {
	project      *types.Project
	services     []string
	fingerprints map[string]string
}

// generateSidecarsZone turns the rendered app project, its main service split off, into the zone:
// on the network shared with the launcher, and naming no app so that two apps with the same
// sidecars get the same containers. It writes the zone compose file, and removes it without sidecars.
func generateSidecarsZone(prj *types.Project, cfg config.Configuration) (sidecarsZone, error) {
	slog.Debug("Generating the sidecars zone", slog.String("app", prj.Name))
	if len(prj.Services) == 0 {
		slog.Debug("No sidecars to run, removing the sidecars zone files")
		return sidecarsZone{fingerprints: map[string]string{}}, removeSidecarsZoneFiles(cfg)
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

	fingerprints, err := sidecarsFingerprints(zone)
	if err != nil {
		return sidecarsZone{}, err
	}
	return sidecarsZone{project: zone, services: zone.ServiceNames(), fingerprints: fingerprints}, nil
}

// sidecarsFingerprints hashes each zone service as compose resolves it, the way compose tells
// whether a container is up to date: a sidecar mounting ${APP_HOME} tells two apps apart.
func sidecarsFingerprints(zone *types.Project) (map[string]string, error) {
	result := make(map[string]string, len(zone.Services))
	for name, svc := range zone.Services {
		data, err := json.Marshal(svc)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		result[name] = hex.EncodeToString(sum[:])
	}
	return result, nil
}

// readSidecarsRecord returns the recorded service fingerprints, nil without record.
func readSidecarsRecord(cfg config.Configuration) map[string]string {
	data, err := sidecarsRecordFile(cfg).ReadFile()
	if err != nil {
		return nil
	}
	var record map[string]string
	if err := json.Unmarshal(data, &record); err != nil {
		return nil
	}
	return record
}

// writeSidecarsRecord records the services the zone containers run, or forgets them when nil.
func writeSidecarsRecord(cfg config.Configuration, fingerprints map[string]string) error {
	file := sidecarsRecordFile(cfg)
	if fingerprints == nil {
		if err := file.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove the sidecars record: %w", err)
		}
		return nil
	}
	if err := file.Parent().MkdirAll(); err != nil {
		return fmt.Errorf("failed to create the sidecars folder: %w", err)
	}
	data, err := json.Marshal(fingerprints)
	if err != nil {
		return err
	}
	return fatomic.WriteFile(file.String(), data, 0o644)
}

func removeSidecarsZoneFiles(cfg config.Configuration) error {
	if err := sidecarsComposeFile(cfg).Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove %s: %w", sidecarsComposeFile(cfg), err)
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

// sidecarsPlan is what a start does to the zone containers.
type sidecarsPlan int

const (
	// sidecarsKeep: every service runs as recorded, only the orphans go
	sidecarsKeep sidecarsPlan = iota
	// sidecarsStart: every service has its container as recorded, some are stopped
	sidecarsStart
	// sidecarsUp: a service has to be created or recreated
	sidecarsUp
)

// planSidecars compares the zone with the recorded fingerprints and the containers.
func planSidecars(zone sidecarsZone, recorded map[string]string, containers appContainers) sidecarsPlan {
	if len(zone.services) > 0 && zone.fingerprints == nil {
		return sidecarsUp
	}
	plan := sidecarsKeep
	for _, service := range zone.services {
		c, ok := containers[service]
		if !ok || recorded[service] == "" || recorded[service] != zone.fingerprints[service] {
			return sidecarsUp
		}
		if c.State != container.StateRunning {
			plan = sidecarsStart
		}
	}
	return plan
}

// only are the containers of the services.
func (c appContainers) only(services []string) appContainers {
	result := make(appContainers, len(services))
	for _, service := range services {
		if ctr, ok := c[service]; ok {
			result[service] = ctr
		}
	}
	return result
}

// orphans are the containers of the services the zone no longer has.
func (c appContainers) orphans(services []string) []container.Summary {
	var result []container.Summary
	for service, ctr := range c {
		if !slices.Contains(services, service) {
			result = append(result, ctr)
		}
	}
	return result
}

// reconcileSidecars brings the zone containers to the zone and returns once they run. When nothing
// is created or recreated, it returns the removal of the orphans instead of running it: they hold
// no port a zone service needs, so they can go while the app starts. A failed removal is retried
// at the next start, since the orphans stay out of the record.
func reconcileSidecars(ctx context.Context, docker command.Cli, cfg config.Configuration, zone sidecarsZone, cb func(StreamMessage)) (removeOrphans func() error, err error) {
	all, err := listAppContainers(ctx, docker.Client())
	if err != nil {
		return nil, err
	}
	containers, legacy := splitAppContainers(all)
	if len(legacy) > 0 {
		slog.Info("removing the sidecars of the per-app projects", slog.Int("containers", len(legacy)))
		if err := removeContainers(ctx, docker.Client(), legacy); err != nil {
			return nil, err
		}
	}

	plan := planSidecars(zone, readSidecarsRecord(cfg), containers)
	slog.Debug("sidecars zone plan", slog.Int("plan", int(plan)), slog.Any("services", zone.services))
	switch plan {
	case sidecarsUp:
		return nil, upSidecars(ctx, docker, cfg, zone, cb)
	case sidecarsStart:
		if err := startAppContainers(ctx, docker.Client(), containers.only(zone.services).notRunning(), cb); err != nil {
			// e.g. the shared network was recreated with the launcher
			slog.Warn("failed to start the existing sidecars, recreating them", slog.String("error", err.Error()))
			if err := removeContainers(ctx, docker.Client(), slices.Collect(maps.Values(containers.only(zone.services)))); err != nil {
				return nil, err
			}
			return nil, upSidecars(ctx, docker, cfg, zone, cb)
		}
	case sidecarsKeep:
	}

	if err := writeSidecarsRecord(cfg, zone.fingerprints); err != nil {
		return nil, err
	}
	orphans := containers.orphans(zone.services)
	if len(orphans) == 0 {
		return nil, nil
	}
	return func() error { return removeContainers(ctx, docker.Client(), orphans) }, nil
}

// upSidecars runs compose up on the zone, which removes the orphans before it creates or recreates
// a service, so that a new service can take the ports of a removed one. It returns once they run.
func upSidecars(ctx context.Context, docker command.Cli, cfg config.Configuration, zone sidecarsZone, cb func(StreamMessage)) error {
	// A failed up leaves the containers half way: the next start runs it again
	if err := writeSidecarsRecord(cfg, nil); err != nil {
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
	return writeSidecarsRecord(cfg, zone.fingerprints)
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
	return errors.Join(writeSidecarsRecord(cfg, nil), removeContainers(ctx, docker, slices.Collect(maps.Values(containers))))
}
