// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pythonImage = "ghcr.io/arduino/app-bricks/python-apps-base:1.0.0"
	influxImage = "influxdb:2.7-alpine"
	modelsImage = "ghcr.io/arduino/app-bricks/models-runner:1.0.0"
)

// keptProject is an app with two bricks: the main service waits for the healthcheck of one
// and for the start of the other.
func keptProject() *types.Project {
	return &types.Project{Name: "my-app", Services: types.Services{
		"main": {Image: pythonImage, DependsOn: types.DependsOnConfig{
			"dbstorage-influx": {Condition: types.ServiceConditionHealthy},
			"models-runner":    {Condition: types.ServiceConditionStarted},
		}},
		"dbstorage-influx": {Image: influxImage},
		"models-runner":    {Image: modelsImage},
	}}
}

// stoppedContainer is the container a stop left behind for a service, listed under the
// name of its image: that name is still the image it was created from.
func stoppedContainer(service, image string) container.Summary {
	labels := map[string]string{}
	if service == "main" {
		labels[DockerAppMainLabel] = "true"
	}
	c := serviceContainer("my-app", service, container.StateExited, labels)
	c.Image = image
	return c
}

func stoppedContainers() []container.Summary {
	return []container.Summary{
		stoppedContainer("dbstorage-influx", influxImage),
		stoppedContainer("main", pythonImage),
		stoppedContainer("models-runner", modelsImage),
	}
}

func TestTheContainersAStopLeftAreStartedAsTheyAre(t *testing.T) {
	kept, reason := keptContainersOf(keptProject(), stoppedContainers())
	require.NotNil(t, kept, reason)

	assert.Equal(t, "my-app-main", kept.main.ID, "the main container is started last")
	waited := map[string]bool{}
	for _, dependency := range kept.dependencies {
		waited[dependency.ID] = dependency.waitHealthy
	}
	assert.Equal(t, map[string]bool{"my-app-dbstorage-influx": true, "my-app-models-runner": false}, waited,
		"the main container waits for the healthcheck of the dependencies compose waits for")

	created := stoppedContainers()
	created[1].State = container.StateCreated
	kept, reason = keptContainersOf(keptProject(), created)
	assert.NotNil(t, kept, "a container that never ran is started too: %s", reason)
}

func TestComposeStartsAnAppWhoseContainersDoNotFitItsProject(t *testing.T) {
	with := func(change func(prj *types.Project, containers []container.Summary) []container.Summary) (*types.Project, []container.Summary) {
		prj, containers := keptProject(), stoppedContainers()
		return prj, change(prj, containers)
	}
	tests := []struct {
		name   string
		change func(prj *types.Project, containers []container.Summary) []container.Summary
	}{
		{name: "a service has no container", change: func(_ *types.Project, c []container.Summary) []container.Summary { return c[1:] }},
		{name: "a container still runs", change: func(_ *types.Project, c []container.Summary) []container.Summary {
			c[0].State = container.StateRunning
			return c
		}},
		{name: "a service has two containers", change: func(_ *types.Project, c []container.Summary) []container.Summary {
			return append(c, stoppedContainer("models-runner", modelsImage))
		}},
		{name: "a container is of another project", change: func(_ *types.Project, c []container.Summary) []container.Summary {
			c[0].Labels[composeProjectLabel] = "other-app"
			return c
		}},
		{name: "a container is of a service the project no longer has", change: func(_ *types.Project, c []container.Summary) []container.Summary {
			return append(c, stoppedContainer("removed-brick", modelsImage))
		}},
		{name: "the image behind a tag was replaced or removed", change: func(_ *types.Project, c []container.Summary) []container.Summary {
			c[1].Image = "sha256:9bfdf7a275b7038ca7722e99ab0be5a4efbe7d6557b016743fe77fcfd9d571e4"
			return c
		}},
		{name: "an image is built at every start", change: func(prj *types.Project, c []container.Summary) []container.Summary {
			service := prj.Services["models-runner"]
			service.Build = &types.BuildConfig{Context: "."}
			prj.Services["models-runner"] = service
			return c
		}},
		{name: "a brick service waits for another", change: func(prj *types.Project, c []container.Summary) []container.Summary {
			service := prj.Services["models-runner"]
			service.DependsOn = types.DependsOnConfig{"dbstorage-influx": {Condition: types.ServiceConditionStarted}}
			prj.Services["models-runner"] = service
			return c
		}},
		{name: "the main service waits for a service to end", change: func(prj *types.Project, c []container.Summary) []container.Summary {
			prj.Services["main"].DependsOn["models-runner"] = types.ServiceDependency{Condition: types.ServiceConditionCompletedSuccessfully}
			return c
		}},
		{name: "no container is the main one", change: func(_ *types.Project, c []container.Summary) []container.Summary {
			delete(c[1].Labels, DockerAppMainLabel)
			return c
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prj, containers := with(tt.change)
			kept, reason := keptContainersOf(prj, containers)
			assert.Nil(t, kept)
			assert.NotEmpty(t, reason)
		})
	}

	kept, reason := keptContainersOf(&types.Project{Name: "my-app"}, nil)
	assert.Nil(t, kept, "a project without services starts nothing")
	assert.NotEmpty(t, reason)
}
