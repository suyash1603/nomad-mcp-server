// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func TestPluginHealthy(t *testing.T) {
	cases := []struct {
		name                                            string
		ctrlRequired                                    bool
		ctrlHealthy, ctrlExpected, nodeHealthy, nodeExp int
		want                                            bool
	}{
		{"all present", true, 1, 1, 3, 3, true},
		{"no controller needed", false, 0, 0, 3, 3, true},
		{"a node instance is down", false, 0, 0, 2, 3, false},
		{"no node instances at all", false, 0, 0, 0, 0, false},
		{"controller required but absent", true, 0, 0, 3, 3, false},
		{"controller required and unhealthy", true, 0, 1, 3, 3, false},
		// A controller that exists but is not required still should not make
		// the plugin unhealthy on its own.
		{"spare controller, not required", false, 0, 1, 3, 3, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pluginHealthy(
				tc.ctrlRequired, tc.ctrlHealthy, tc.ctrlExpected, tc.nodeHealthy, tc.nodeExp))
		})
	}
}

func TestPluginProblemExplainsTheConsequence(t *testing.T) {
	t.Run("no node instances", func(t *testing.T) {
		require.Contains(t, pluginProblem(false, 0, 0, 0, 0), "no allocation can mount")
	})

	t.Run("missing controller", func(t *testing.T) {
		require.Contains(t, pluginProblem(true, 0, 0, 3, 3), "cannot be attached")
	})

	t.Run("unhealthy controllers", func(t *testing.T) {
		msg := pluginProblem(true, 1, 3, 3, 3)
		require.Contains(t, msg, "2 of 3 controllers")
		require.Contains(t, msg, "cluster-wide")
	})

	t.Run("unhealthy nodes name the consequence", func(t *testing.T) {
		msg := pluginProblem(false, 0, 0, 1, 4)
		require.Contains(t, msg, "3 of 4")
		require.Contains(t, msg, "cannot be mounted on those nodes")
	})

	t.Run("healthy plugin has no problem", func(t *testing.T) {
		require.Empty(t, pluginProblem(true, 1, 1, 3, 3))
	})
}

func TestRatio(t *testing.T) {
	require.Equal(t, "2/3 healthy", ratio(2, 3))
	require.Equal(t, "0/1 healthy", ratio(0, 1))
	require.Equal(t, "none registered", ratio(0, 0))
}

// TestUnhealthyInstancesReturnsOnlyTheUnhealthy is what keeps this readable on
// a large cluster: two hundred healthy fingerprints say nothing and would bury
// the handful that matter.
func TestUnhealthyInstancesReturnsOnlyTheUnhealthy(t *testing.T) {
	in := map[string]*api.CSIInfo{
		"node-aaaa-1111-2222-3333-444444444444": {Healthy: true},
		"node-bbbb-1111-2222-3333-444444444444": {Healthy: false, HealthDescription: "mount failed"},
		"node-cccc-1111-2222-3333-444444444444": nil,
	}

	got := unhealthyInstances("node", in)

	require.Len(t, got, 1)
	require.Equal(t, "node", got[0].Kind)
	require.Equal(t, "mount failed", got[0].Why)
	require.False(t, got[0].Healthy)
}

func TestUnhealthyInstancesFillsInAMissingReason(t *testing.T) {
	got := unhealthyInstances("controller", map[string]*api.CSIInfo{
		"n1": {Healthy: false, AllocID: "alloc-1111-2222-3333-444444444444"},
	})

	require.Len(t, got, 1)
	// An empty reason is the common case and the least helpful, so it is
	// replaced with where to look instead.
	require.Contains(t, got[0].Why, "allocation's logs")
	require.NotEmpty(t, got[0].AllocID)
}

func TestUnhealthyInstancesReportsAge(t *testing.T) {
	got := unhealthyInstances("node", map[string]*api.CSIInfo{
		"n1": {Healthy: false, UpdateTime: time.Now().Add(-2 * time.Hour)},
	})
	require.NotEmpty(t, got[0].Updated)
}

func TestProjectPluginSortsAndSummarises(t *testing.T) {
	plugin := &api.CSIPlugin{
		ID:                  "ebs",
		Provider:            "ebs.csi.aws.com",
		ControllerRequired:  true,
		ControllersHealthy:  1,
		ControllersExpected: 1,
		NodesHealthy:        2,
		NodesExpected:       3,
		Nodes: map[string]*api.CSIInfo{
			"zzzz": {Healthy: false, HealthDescription: "z broke"},
		},
		Controllers: map[string]*api.CSIInfo{
			"aaaa": {Healthy: false, HealthDescription: "a broke"},
		},
	}

	got := projectPlugin(plugin)

	require.False(t, got.Healthy)
	require.Equal(t, "2/3 healthy", got.Nodes)
	require.Equal(t, "1/1 healthy", got.Controllers)
	require.Len(t, got.Unhealthy, 2)
	// Controllers before nodes: a broken controller affects the whole cluster,
	// a broken node instance only its own node.
	require.Equal(t, "controller", got.Unhealthy[0].Kind)
	require.Equal(t, "node", got.Unhealthy[1].Kind)
	require.Contains(t, got.Note, "list_job_allocations")
}

func TestProjectPluginHealthyRedirects(t *testing.T) {
	got := projectPlugin(&api.CSIPlugin{
		ID: "ebs", NodesHealthy: 3, NodesExpected: 3,
	})

	require.True(t, got.Healthy)
	require.Empty(t, got.Problem)
	// A healthy plugin still needs to say where to look next, or the reader
	// concludes the volume problem is unexplainable.
	require.Contains(t, got.Note, "diagnose_volume")
}

func TestProjectPluginWithNoNodeInstances(t *testing.T) {
	got := projectPlugin(&api.CSIPlugin{ID: "ebs"})

	require.False(t, got.Healthy)
	require.Equal(t, "none registered", got.Nodes)
	require.Contains(t, got.Note, "system job")
}

func TestProjectPluginNilIsSafe(t *testing.T) {
	require.Equal(t, pluginDetail{}, projectPlugin(nil))
}
