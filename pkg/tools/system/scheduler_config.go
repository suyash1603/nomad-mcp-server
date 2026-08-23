// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package system

import (
	"context"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// schedulerConfig is the projection returned by get_scheduler_config.
type schedulerConfig struct {
	SchedulerAlgorithm string `json:"scheduler_algorithm"`

	PreemptionSystem   bool `json:"preemption_system_jobs"`
	PreemptionSysBatch bool `json:"preemption_sysbatch_jobs"`
	PreemptionBatch    bool `json:"preemption_batch_jobs"`
	PreemptionService  bool `json:"preemption_service_jobs"`

	MemoryOversubscription bool `json:"memory_oversubscription_enabled"`
	RejectJobRegistration  bool `json:"reject_job_registration"`
	PauseEvalBroker        bool `json:"pause_eval_broker"`

	NodeLimitForFeasibilityChecks uint `json:"node_limit_for_feasibility_checks,omitempty"`

	ModifyIndex uint64 `json:"modify_index"`

	Warnings []string `json:"warnings,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// GetSchedulerConfig reads the cluster-wide scheduler configuration.
func GetSchedulerConfig(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("get_scheduler_config",
			mcp.WithDescription(
				"Read the cluster-wide scheduler configuration: the placement algorithm, which "+
					"job types may preempt lower-priority work, whether memory oversubscription is "+
					"on, and the two switches that stop the cluster accepting or processing work.\n\n"+
					"Two settings here explain otherwise baffling cluster-wide symptoms, and are "+
					"worth checking before a long investigation. reject_job_registration makes "+
					"every job submission fail for anyone without a management token. "+
					"pause_eval_broker stops evaluations being processed at all, so jobs are "+
					"accepted and then simply never placed — no error, no allocation, nothing.\n\n"+
					"Preemption settings explain the opposite symptom: a low-priority allocation "+
					"that was evicted for no apparent reason was preempted by a higher-priority "+
					"job.\n\n"+
					"scheduler_algorithm = \"spread\" and memory_oversubscription_enabled are Nomad "+
					"Enterprise features; on Community Edition they read as their defaults."),
			utils.ReadOnlyTool(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			resp, _, err := nomad.Operator().SchedulerGetConfiguration(&api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the scheduler configuration",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if resp == nil || resp.SchedulerConfig == nil {
				return utils.ErrorResult("Nomad returned no scheduler configuration.")
			}

			c := resp.SchedulerConfig
			out := schedulerConfig{
				SchedulerAlgorithm:            string(c.SchedulerAlgorithm),
				PreemptionSystem:              c.PreemptionConfig.SystemSchedulerEnabled,
				PreemptionSysBatch:            c.PreemptionConfig.SysBatchSchedulerEnabled,
				PreemptionBatch:               c.PreemptionConfig.BatchSchedulerEnabled,
				PreemptionService:             c.PreemptionConfig.ServiceSchedulerEnabled,
				MemoryOversubscription:        c.MemoryOversubscriptionEnabled,
				RejectJobRegistration:         c.RejectJobRegistration,
				PauseEvalBroker:               c.PauseEvalBroker,
				NodeLimitForFeasibilityChecks: c.NodeLimitForFeasibilityChecks,
				ModifyIndex:                   c.ModifyIndex,
			}

			if c.RejectJobRegistration {
				out.Warnings = append(out.Warnings,
					"reject_job_registration is ON. Every job submission from a non-management "+
						"token is being refused cluster-wide. If someone reports that they cannot "+
						"deploy anything, this is why.")
			}
			if c.PauseEvalBroker {
				out.Warnings = append(out.Warnings,
					"pause_eval_broker is ON. Evaluations are not being processed, so jobs are "+
						"accepted and then never placed — no error is reported anywhere. This "+
						"explains a cluster where nothing schedules and nothing appears wrong.")
			}
			if c.PreemptionConfig.ServiceSchedulerEnabled {
				out.Warnings = append(out.Warnings,
					"Service job preemption is enabled: a higher-priority service job may evict a "+
						"running lower-priority one. An allocation that stopped for no visible "+
						"reason may have been preempted; read_evaluation on the job says so.")
			}

			out.Note = "This is cluster-wide configuration. Node pools may override the algorithm " +
				"and memory oversubscription individually on Enterprise; read_node_pool shows that."

			return utils.JSONResult(out)
		},
	}
}

// SetSchedulerConfig changes the cluster-wide scheduler configuration.
func SetSchedulerConfig(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("set_scheduler_config",
			mcp.WithDescription(
				"Change the cluster-wide scheduler configuration.\n\n"+
					"Every setting here applies to the entire cluster at once, so a mistake affects "+
					"every job in it rather than one workload. Two of the switches can stop the "+
					"cluster working altogether: reject_job_registration refuses all job "+
					"submissions, and pause_eval_broker stops evaluations being processed so that "+
					"nothing is ever placed. Both are legitimate during maintenance and both look "+
					"like a total outage to everyone else.\n\n"+
					"Turning preemption ON allows the scheduler to EVICT running lower-priority "+
					"allocations to make room for higher-priority ones. That interrupts work that "+
					"was running perfectly well, so it is a change to make deliberately and never "+
					"as a way to get one job placed.\n\n"+
					"Only the settings you pass are changed; anything omitted keeps its current "+
					"value. Read the current state with get_scheduler_config first, show the user "+
					"exactly what changes, and get explicit confirmation. This is not a per-job "+
					"knob and should not be used as one.\n\n"+
					"scheduler_algorithm = \"spread\" and memory_oversubscription require Nomad "+
					"Enterprise."),
			// Destructive: enabling preemption can evict running allocations,
			// and either pause switch stops the cluster placing work.
			utils.MutatingTool(true, true),
			mcp.WithString("scheduler_algorithm",
				mcp.Enum("binpack", "spread"),
				mcp.Description(
					"\"binpack\" packs work onto as few nodes as possible; \"spread\" distributes it "+
						"evenly. \"spread\" requires Nomad Enterprise."),
			),
			mcp.WithBoolean("memory_oversubscription",
				mcp.Description(
					"Allow tasks to exceed their memory reservation up to memory_max. Requires "+
						"Nomad Enterprise."),
			),
			mcp.WithBoolean("preemption_system_jobs",
				mcp.Description("Allow system jobs to preempt lower-priority allocations."),
			),
			mcp.WithBoolean("preemption_sysbatch_jobs",
				mcp.Description("Allow sysbatch jobs to preempt lower-priority allocations."),
			),
			mcp.WithBoolean("preemption_batch_jobs",
				mcp.Description("Allow batch jobs to preempt lower-priority allocations."),
			),
			mcp.WithBoolean("preemption_service_jobs",
				mcp.Description(
					"Allow service jobs to preempt lower-priority allocations. This is the one "+
						"most likely to interrupt production work."),
			),
			mcp.WithBoolean("reject_job_registration",
				mcp.Description(
					"When true, every job submission from a non-management token is refused, "+
						"cluster-wide. Intended for maintenance windows."),
			),
			mcp.WithBoolean("pause_eval_broker",
				mcp.Description(
					"When true, evaluations stop being processed and nothing is scheduled at all. "+
						"Jobs are still accepted, so this looks like silence rather than an error. "+
						"Intended for incident response on an overloaded leader."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			q := &api.QueryOptions{Region: region}

			// The API replaces the whole configuration, so the current state is
			// read first and only the named fields are overwritten. Without
			// this, omitting an argument would silently reset that setting.
			current, _, err := nomad.Operator().SchedulerGetConfiguration(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read the current scheduler configuration",
					Address:    p.Address(),
					Capability: "operator:read",
				}, p.Redactor()))
			}
			if current == nil || current.SchedulerConfig == nil {
				return utils.ErrorResult(
					"Nomad returned no current scheduler configuration, so it cannot be safely " +
						"modified: a write would have to guess at the settings not being changed.")
			}

			cfg := current.SchedulerConfig
			args := req.GetArguments()
			changes := map[string]any{}

			if _, ok := args["scheduler_algorithm"]; ok {
				v := req.GetString("scheduler_algorithm", "")
				switch v {
				case "binpack", "spread":
					if string(cfg.SchedulerAlgorithm) != v {
						changes["scheduler_algorithm"] = string(cfg.SchedulerAlgorithm) + " -> " + v
					}
					cfg.SchedulerAlgorithm = api.SchedulerAlgorithm(v)
				default:
					return utils.ErrorResultf(
						"Invalid scheduler_algorithm %q: it must be \"binpack\" or \"spread\".", v)
				}
			}

			setBool := func(arg string, field *bool) {
				if _, ok := args[arg]; !ok {
					return
				}
				v := req.GetBool(arg, *field)
				if *field != v {
					changes[arg] = boolArrow(*field, v)
				}
				*field = v
			}
			setBool("memory_oversubscription", &cfg.MemoryOversubscriptionEnabled)
			setBool("preemption_system_jobs", &cfg.PreemptionConfig.SystemSchedulerEnabled)
			setBool("preemption_sysbatch_jobs", &cfg.PreemptionConfig.SysBatchSchedulerEnabled)
			setBool("preemption_batch_jobs", &cfg.PreemptionConfig.BatchSchedulerEnabled)
			setBool("preemption_service_jobs", &cfg.PreemptionConfig.ServiceSchedulerEnabled)
			setBool("reject_job_registration", &cfg.RejectJobRegistration)
			setBool("pause_eval_broker", &cfg.PauseEvalBroker)

			if len(changes) == 0 {
				return utils.JSONResult(map[string]any{
					"changed": false,
					"note": "Nothing was changed: every setting given already had the value " +
						"requested, or no setting was given. get_scheduler_config shows the current " +
						"configuration.",
				})
			}

			// A compare-and-swap on the modify index turns a concurrent change
			// by another operator into a refusal rather than a silent
			// overwrite of settings this call never intended to touch.
			resp, _, err := nomad.Operator().SchedulerCASConfiguration(cfg, &api.WriteOptions{
				Region: region,
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "update the scheduler configuration",
					Address:    p.Address(),
					Capability: "operator:write",
				}, p.Redactor()))
			}
			if resp != nil && !resp.Updated {
				return utils.ErrorResult(
					"The scheduler configuration was NOT updated: it changed underneath this call, " +
						"so the write was refused rather than overwriting whatever the other change " +
						"was. Read it again with get_scheduler_config and retry if the change is " +
						"still wanted.")
			}

			out := map[string]any{
				"changed": true,
				"changes": changes,
				"note": "The scheduler configuration was updated cluster-wide. Confirm with " +
					"get_scheduler_config.",
			}
			if cfg.RejectJobRegistration {
				out["warning"] = "reject_job_registration is now ON: every job submission from a " +
					"non-management token is being refused, cluster-wide. Remember to turn it off."
			}
			if cfg.PauseEvalBroker {
				out["warning"] = "pause_eval_broker is now ON: nothing at all is being scheduled, " +
					"and jobs submitted meanwhile are accepted without ever being placed. Remember " +
					"to turn it off."
			}
			return utils.JSONResult(out)
		},
	}
}

func boolArrow(from, to bool) string {
	return boolStr(from) + " -> " + boolStr(to)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
