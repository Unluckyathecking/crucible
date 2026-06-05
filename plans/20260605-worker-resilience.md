# Plan: worker-resilience

Sprint plan file. See PR body for full spec.

- Repo: crucible
- Branch: ultra-plan/20260605-worker-resilience
- Date: 2026-06-05

Gateway-to-worker resilience: bounded retry (transport/5xx only, never a 200)
plus a circuit-breaker, behind the existing `proxy.Client` seam. Reusable
infrastructure every clone inherits. Downstream `10xworker:job` decomposes the
subunits per the PR JSON spec.
