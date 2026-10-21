// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// The servicelocator pkg should be used only under cmd/arduino-app-cli as a convenience to build our DI.

package servicelocator

import (
	"sync"

	dockerCommand "github.com/docker/cli/cli/command"
	"go.bug.st/f"

	"github.com/arduino/arduino-app-cli/internal/dockerhelper"
	"github.com/arduino/arduino-app-cli/internal/orchestrator"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/appid"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricks"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/config"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/modelsindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/servicesindex"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

var globalConfig config.Configuration

func Init(cfg config.Configuration) {
	globalConfig = cfg
}

var (
	// The indexes read the asset folder, which the provisioner writes: whoever asks first at boot waits for it
	GetBricksIndex = sync.OnceValue(func() *bricksindex.BricksIndex {
		GetProvisioner()
		return f.Must(bricksindex.Load(GetPlatform(), globalConfig.AssetDir()))
	})

	GetModelsIndex = sync.OnceValue(func() *modelsindex.ModelsIndex {
		GetProvisioner()
		return f.Must(modelsindex.Load(GetPlatform(), globalConfig.AssetDir(), globalConfig.ModelsDir(), globalConfig.CustomModelsDir(), GetDockerClient().Client(), globalConfig))
	})

	GetServicesIndex = sync.OnceValue(func() *servicesindex.ServicesIndex {
		GetProvisioner()
		return f.Must(servicesindex.Load(GetPlatform(), globalConfig.AssetDir().Join("services")))
	})

	GetProvisioner = sync.OnceValue(func() *orchestrator.Provision {
		return f.Must(orchestrator.NewProvision(
			GetDockerClient(),
			globalConfig,
		))
	})

	docker *dockerCommand.DockerCli

	GetDockerClient = sync.OnceValue(func() *dockerCommand.DockerCli {
		docker = f.Must(dockerhelper.NewCli())
		return docker
	})

	CloseDockerClient = func() error {
		if docker != nil {
			return docker.Client().Close()
		}
		return nil
	}

	GetBrickService = sync.OnceValue(func() *bricks.Service {
		return bricks.NewService(
			GetModelsIndex(),
			GetBricksIndex(),
		)
	})

	GetAppIDProvider = sync.OnceValue(func() *appid.Provider {
		return appid.NewAppProvider(globalConfig, GetPlatform())
	})

	GetPlatform = sync.OnceValue(func() platform.Platform {
		return platform.GetPlatform(globalConfig.DataDir())
	})
)
