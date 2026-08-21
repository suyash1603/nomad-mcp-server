# Copyright (c) 2026 suyash1603
# SPDX-License-Identifier: MPL-2.0
#
# A batch job that runs to completion, plus a task that fails.
#
# Two groups on purpose:
#   "report"  succeeds and exits 0, so the job reaches "complete"
#   "flaky"   exits non-zero every time, exhausting its restarts and ending
#             "failed", with the reason visible only in the task's stderr
#
# That mix is what makes this useful for exercising the debugging path: the job
# summary shows both a completed and a failed group, and answering "why did it
# fail?" requires read_allocation_logs against the failing task's stderr.
#
#   nomad job run examples/batch-report.nomad.hcl

job "batch-report" {
  type = "batch"

  meta {
    purpose = "nomad-mcp-server example: batch work, one group succeeding and one failing"
  }

  group "report" {
    count = 1

    task "generate" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args = ["-c", <<-EOT
          echo "generating report..."
          for i in 1 2 3; do
            echo "  processed batch $i of 3"
            sleep 1
          done
          echo "report complete"
          exit 0
        EOT
        ]
      }

      resources {
        cpu    = 100
        memory = 32
      }
    }
  }

  group "flaky" {
    count = 1

    # Few attempts and no rescheduling, so it settles into "failed" quickly
    # rather than retrying for minutes.
    restart {
      attempts = 1
      interval = "1m"
      delay    = "3s"
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "process" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args = ["-c", <<-EOT
          echo "starting processing job"
          echo "connecting to upstream database..."
          sleep 1
          echo "ERROR: could not connect to database at db.internal:5432: connection refused" >&2
          echo "ERROR: giving up after 1 attempt" >&2
          exit 1
        EOT
        ]
      }

      resources {
        cpu    = 100
        memory = 32
      }
    }
  }
}
