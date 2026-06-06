# 10xworker:job — gateway-runtime-assembly

See PR body for the authoritative JSON spec. This file marks the sprint-planner
job branch; the implementing 10x worker owns all `gateway/internal/runtime/*`
source. Planner does not write implementation code.

Module: gateway-runtime-assembly
Scope: gateway/internal/runtime/{assembly.go,assembly_test.go,doc.go}
Purpose: config-driven assembly of the dormant #101 resilience policy + #103
tracer provider + composite shutdown, WITHOUT editing main.go (parallel-safe
with #48) or ci.yml (#102). Net-new package only.
