# Copyright (c) 2026 suyash1603
# SPDX-License-Identifier: MPL-2.0
#
# Configuration for the throwaway Nomad agent used to test this MCP server:
#
#   nomad agent -dev -bind 127.0.0.1 -config scripts/dev-agent.hcl
#
# Why this file exists at all:
#
# On Apple Silicon, Nomad mis-fingerprints CPU frequency. On an M4 Pro it
# reports cpu.frequency.performance = 4 (MHz, not GHz), which works out to
# roughly 40 MHz of total allocatable compute for the whole node. Any job
# asking for a realistic amount of CPU — 100 MHz is a normal, small request —
# is then unschedulable, and Nomad reports:
#
#   * Resources exhausted on 1 nodes
#   * Dimension "cpu" exhausted on 1 nodes
#
# which looks exactly like a broken example jobspec rather than a fingerprinting
# problem. Setting cpu_total_compute overrides the fingerprint so the examples in
# examples/ place normally.
#
# Verified against Nomad 2.0.4 on an Apple M4 Pro, 2026-08-20.
#
# This is a *test* agent. Nothing here is a recommendation for a real cluster.

client {
  # Override the mis-fingerprinted CPU capacity. Any comfortably large value
  # works; this is roughly what 12 cores should actually report.
  cpu_total_compute = 24000
}

# Keep the dev agent quiet enough to read alongside the MCP server's own logs.
log_level = "WARN"
