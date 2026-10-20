// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/arduino/arduino-app-cli/internal/fatomic"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
)

// runScript is the entrypoint of the python image: it runs the app, or serves the supervisor.
const runScript = "/run.sh"

// appProject is the compose project of an app as rendered on this board, its main service turned
// into the warm container: the supervisor serves in it and the app environment travels with each
// start instead. The project files stay as they are generated, and frozen in a release.
type appProject struct {
	prj *types.Project
	// fingerprint tells the project apart from the one the containers were created from.
	fingerprint string
	// appEnvs is what the main service would have run the app with: the app half of a start.
	appEnvs map[string]string
}

// adaptAppProject turns the main service of the rendered project into the warm container of the
// app: nothing of it changes between two starts of the app but its ports, so that the container
// is recreated only for them. The sidecars are left as the project states them.
func adaptAppProject(prj *types.Project, c appContainer, cfg config.Configuration, boardEnvs types.Mapping) (appProject, error) {
	main, ok := prj.Services[mainServiceName]
	if !ok {
		return appProject{}, fmt.Errorf("the compose project of the app has no %s service", mainServiceName)
	}
	appEnvs := make(map[string]string, len(main.Environment))
	for k, v := range main.Environment {
		if v != nil {
			appEnvs[k] = *v
		}
	}

	envs := boardEnvs.Clone()
	envs["APP_LAUNCHER_PREPARE_TIMEOUT_S"] = strconv.Itoa(int(launcherPrepareTimeout.Seconds()))
	if cfg.LauncherPreload != "" {
		envs["APP_LAUNCHER_PRELOAD"] = cfg.LauncherPreload
	}
	grace := types.Duration(10 * time.Second)
	// The template lists the ports in no fixed order: sorted, the same app gives the same fingerprint
	slices.SortFunc(main.Ports, func(a, b types.ServicePortConfig) int {
		return cmp.Or(cmp.Compare(a.Published, b.Published), cmp.Compare(a.Target, b.Target))
	})
	main.Entrypoint = types.ShellCommand{runScript, "serve"}
	main.Command = nil
	main.WorkingDir = appMountDir
	main.Restart = types.RestartPolicyNo
	main.StopGracePeriod = &grace
	// A healthcheck would run a python in every warm container; the sidecars are waited for by the bricks
	main.HealthCheck = nil
	main.DependsOn = nil
	main.Environment = envs.ToMappingWithEquals()
	main.Volumes = append(main.Volumes, types.ServiceVolumeConfig{Type: types.VolumeTypeBind, Source: c.runtimeDir(cfg).String(), Target: appSocketDir})
	prj.Services[mainServiceName] = main

	fingerprint, err := projectFingerprint(prj)
	if err != nil {
		return appProject{}, err
	}
	return appProject{prj: prj, fingerprint: fingerprint, appEnvs: appEnvs}, nil
}

// ports are the host ports the main service publishes.
func (p appProject) ports() []int {
	var ports []int
	for _, port := range p.prj.Services[mainServiceName].Ports {
		if published, err := strconv.Atoi(port.Published); err == nil {
			ports = append(ports, published)
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

// services are the names of the project services, the main one included.
func (p appProject) services() []string {
	return p.prj.ServiceNames()
}

// projectFingerprint hashes the services as compose resolves them, the way compose tells whether
// a container is up to date.
func projectFingerprint(prj *types.Project) (string, error) {
	data, err := json.Marshal(prj.Services)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// projectRecord holds the fingerprint of the project the containers of the app were created from.
type projectRecord struct {
	Fingerprint string `json:"fingerprint"`
}

func (c appContainer) recordFile(cfg config.Configuration) *paths.Path {
	return c.dir(cfg).Join("project.json")
}

// recordedFingerprint is the fingerprint of the containers, empty without record.
func (c appContainer) recordedFingerprint(cfg config.Configuration) string {
	data, err := c.recordFile(cfg).ReadFile()
	if err != nil {
		return ""
	}
	var record projectRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return ""
	}
	return record.Fingerprint
}

// recordFingerprint notes which project the containers were created from.
func (c appContainer) recordFingerprint(cfg config.Configuration, fingerprint string) error {
	data, err := json.Marshal(projectRecord{Fingerprint: fingerprint})
	if err != nil {
		return err
	}
	file := c.recordFile(cfg)
	if err := file.Parent().MkdirAll(); err != nil {
		return err
	}
	return fatomic.WriteFile(file.String(), data, 0o644)
}
