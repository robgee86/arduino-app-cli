// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	dockerClient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/orchestrator/app"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/bricksindex"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/modelsindex"
	"github.com/arduino/arduino-app-cli/internal/platform"
)

func TestPrepareModelsSkipsWhatItShould(t *testing.T) {
	bricksIndex := &bricksindex.BricksIndex{
		BuiltInBricks: []bricksindex.Brick{
			{
				ID:           "arduino:ai-brick",
				Name:         "AI brick with a default model",
				RequireModel: true,
				ModelName:    "installed-default-model",
			},
			{
				ID:           "arduino:ai-brick-override",
				Name:         "AI brick whose model the app overrides",
				RequireModel: true,
				ModelName:    "installed-default-model",
			},
			{
				ID:           "arduino:ai-brick-builtin",
				Name:         "AI brick wired to a model the handler does not download",
				RequireModel: true,
				ModelName:    "builtin-model",
			},
			{
				ID:           "arduino:ai-brick-unknown",
				Name:         "AI brick wired to a model the index does not know",
				RequireModel: true,
				ModelName:    "unknown-model",
			},
			{
				ID:   "arduino:plain-brick",
				Name: "Brick that requires no model",
			},
		},
	}

	modelIndex := &modelsindex.ModelsIndex{
		InternalModels: []modelsindex.AIModel{
			{
				ID:         "installed-default-model",
				Status:     modelsindex.InstalledStatus,
				Deployment: &modelsindex.ModelDeployment{Handler: "a-handler"},
				Bricks:     []modelsindex.BrickConfig{{ID: "arduino:ai-brick"}},
			},
			{
				ID:         "installed-override-model",
				Status:     modelsindex.InstalledStatus,
				Deployment: &modelsindex.ModelDeployment{Handler: "a-handler"},
				Bricks:     []modelsindex.BrickConfig{{ID: "arduino:ai-brick-override"}},
			},
			{
				// A pre-loaded model is there by declaration: NeedsNoDownload short-circuits
				// its install even though it reads as not installed.
				ID:         "builtin-model",
				Status:     modelsindex.NotInstalledStatus,
				Deployment: &modelsindex.ModelDeployment{PreLoaded: true},
				Bricks:     []modelsindex.BrickConfig{{ID: "arduino:ai-brick-builtin"}},
			},
		},
	}

	appBricks := []app.Brick{
		{ID: "arduino:ai-brick"},
		{ID: "arduino:ai-brick-override", Model: "installed-override-model"},
		{ID: "arduino:ai-brick-builtin"},
		{ID: "arduino:ai-brick-unknown"},
		{ID: "arduino:plain-brick"},
	}

	var messages []string
	cb := func(m StreamMessage) {
		if m.GetType() == InfoType {
			messages = append(messages, m.GetData())
		}
	}

	err := prepareModels(t.Context(), nil, appBricks, bricksIndex, modelIndex, platform.Platform{}, cb)
	require.NoError(t, err)

	// The installed and the unresolved models are downloaded nothing about: only the
	// pre-loaded one reaches the install, which then finds it needs no download.
	assert.NotContains(t, messages, "downloading the model installed-default-model")
	assert.NotContains(t, messages, "downloading the model installed-override-model")
	assert.NotContains(t, messages, "downloading the model unknown-model")
}

func TestReleaseImages(t *testing.T) {
	cfg := setTestOrchestratorConfig(t)
	prj := &types.Project{Services: types.Services{
		"main":   {Name: "main", Image: "ghcr.io/arduino/app-bricks/python-apps-base:0.12.0"},
		"runner": {Name: "runner", Image: "ghcr.io/arduino/app-bricks/ei-models-runner:0.12.0"},
	}}
	// The launcher runs the release: the image its build froze for the main service is not pulled
	assert.ElementsMatch(t, []string{cfg.PythonImage, "ghcr.io/arduino/app-bricks/ei-models-runner:0.12.0"}, releaseImages(prj, cfg))
}

// inspectOnlyDocker answers image inspects only: a listing of the images panics.
type inspectOnlyDocker struct {
	dockerClient.APIClient
	present   []string
	inspected []string
}

func (d *inspectOnlyDocker) ImageInspect(_ context.Context, image string, _ ...dockerClient.ImageInspectOption) (dockerClient.ImageInspectResult, error) {
	d.inspected = append(d.inspected, image)
	if slices.Contains(d.present, image) {
		return dockerClient.ImageInspectResult{}, nil
	}
	return dockerClient.ImageInspectResult{}, errors.New("no such image")
}

func (d *inspectOnlyDocker) Ping(context.Context, dockerClient.PingOptions) (dockerClient.PingResult, error) {
	return dockerClient.PingResult{}, errors.New("docker daemon not reachable")
}

func TestCheckImagesInspectsOnlyTheImagesOfTheApp(t *testing.T) {
	docker := &inspectOnlyDocker{present: []string{"runner:1"}}
	cli, err := command.NewDockerCli(command.WithAPIClient(docker), command.WithBaseContext(t.Context()))
	require.NoError(t, err)
	require.NoError(t, cli.Initialize(&flags.ClientOptions{}))

	present := &types.Project{Services: types.Services{
		"runner": {Name: "runner", Image: "runner:1"},
		"other":  {Name: "other", Image: "runner:1"},
	}}
	require.NoError(t, checkImages(t.Context(), cli, present))
	assert.Equal(t, []string{"runner:1"}, docker.inspected, "once per image, no listing")

	missing := &types.Project{Services: types.Services{"db": {Name: "db", Image: "db:2"}}}
	require.ErrorIs(t, checkImages(t.Context(), cli, missing), ErrNotPrepared)

	docker.inspected = nil
	require.NoError(t, checkImages(t.Context(), cli, &types.Project{}))
	assert.Empty(t, docker.inspected, "an app without sidecars asks nothing")
}
