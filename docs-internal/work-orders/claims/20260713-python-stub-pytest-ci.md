# worker:claim ci — run the never-executed Python worker-stub pytest suite

**Repo / area:** crucible · `.github/workflows/worker-conformance.yml` (Python matrix leg)

**Change.** In the Python leg, add a step to install `pytest` and run
`workers/stubs/python/test_worker.py` (e.g. `pip install pytest && python3 -m pytest test_worker.py`
with `working-directory: workers/stubs/python`), alongside the existing SDK
fixture-conformance step.

**Expected outcome.** The 7 shipped `def test_*` functions in the stub actually execute in CI
instead of being dead coverage.

**Verified gap.** `workers/stubs/python/test_worker.py` has 7 pytest-style tests and
`import pytest` at line 19. The only Python CI step (`worker-conformance.yml:88`) runs
`python3 -m unittest conformance.test_fixture_conformance` in `workers/sdk-python` — it never
touches `workers/stubs/python/test_worker.py`. `smoke-new-tool.sh` / `conformance-run.sh` only
`py_compile` and launch `worker.py`, never run its tests. These tests need pytest (not
`unittest`), which isn't installed, so they have zero execution path today.

**Files / constraints.** Single-workflow-file edit; additive step only. Do NOT alter the Go,
Rust, or TS legs, the SDK fixture-conformance step, or any other workflow. Parallel-safe with
the crucible primary job (source vs CI workflow, disjoint files).
