# Copyright (c) 2026 suyash1603
# SPDX-License-Identifier: MPL-2.0
#
# A healthy long-running service job — the happy path.
#
# Uses the raw_exec driver so it runs on a plain `nomad agent -dev` on macOS or
# Linux with no Docker daemon and no images to pull. `nomad agent -dev` enables
# raw_exec automatically; a production agent does not, and should not.
#
#   nomad job run examples/hello-service.nomad.hcl

job "hello-service" {
  type = "service"

  meta {
    owner   = "platform-team"
    purpose = "nomad-mcp-server example: a job that runs and stays healthy"
  }

  group "web" {
    count = 2

    # Restart quickly so a deliberately broken variant surfaces its failure
    # without a long wait.
    restart {
      attempts = 2
      interval = "1m"
      delay    = "5s"
      mode     = "fail"
    }

    task "server" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args = ["-c", <<-EOT
          echo "hello-service starting on $(hostname)"
          i=0
          while true; do
            i=$((i+1))
            echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] heartbeat $i"
            if [ $((i % 5)) -eq 0 ]; then
              echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] periodic notice on stderr" >&2
            fi
            sleep 5
          done
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
