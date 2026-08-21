# Copyright (c) 2026 suyash1603
# SPDX-License-Identifier: MPL-2.0
#
# A job that can never be placed — the troubleshooting path.
#
# It asks for a datacenter that does not exist, a node class that does not
# exist, and far more memory than any dev agent has. Nomad accepts the job,
# creates an evaluation, and leaves it blocked with placement failures.
#
# This is the interesting case for an AI client: the job exists, it is not
# "broken" in any syntactic sense, and the explanation lives in the evaluation's
# FailedTGAllocs rather than anywhere in the job itself. Reading `read_job` will
# not tell you why. `list_job_evaluations` then `read_evaluation` will.
#
#   nomad job run examples/unplaceable.nomad.hcl
#   # then ask: "why is the unplaceable job not running?"

job "unplaceable" {
  type = "service"

  # No such datacenter exists in a dev agent, which uses "dc1".
  datacenters = ["dc-does-not-exist"]

  meta {
    purpose = "nomad-mcp-server example: a job that cannot be scheduled"
  }

  group "impossible" {
    count = 1

    constraint {
      attribute = "${node.class}"
      value     = "gpu-node-that-does-not-exist"
    }

    task "server" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args    = ["-c", "echo 'this will never run'; sleep 3600"]
      }

      # More memory than any laptop dev agent will offer.
      resources {
        cpu    = 100000
        memory = 1000000
      }
    }
  }
}
