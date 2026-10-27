---
description: VTX delivery chain — vtx-coder implements and commits, vtx-qa independently verifies the exact SHA, vtx-reviewer audits; then the orchestrator gates and opens a Draft PR.
---

Drive one VTX run end to end for: $@

Delegate with the native `subagent` tool, one foreground child per call (`agentScope` and the working directory are pinned to the run worktree by the orchestrator; `workflow`, `chain`, `tasks`, `async`, and `model` are rejected). The specialists are project-local agents under `.pi/agents/`.

1. **vtx-coder** — implement `$@` against the acceptance criteria in the run spec. Run the local test baseline, then commit. Output the exact commit SHA and the files changed.
2. **vtx-reviewer** — statically review the change at that commit for correctness, security, and convention adherence. Emit findings and a `recommendation`.

After delegation, the orchestrator (not a subagent) continues through the spine, which is the only authority for state:

3. `vtx_verify` — record verification evidence for the committed SHA.
4. `vtx_qa` — launch the independent QA session for that exact SHA. The QA specialist submits its verdict through its own spine access; you cannot author it.
5. `vtx_gate` — evaluate the quality gate. This takes NO verdict argument; the spine recomputes PASS/FAIL from committed evidence. You cannot assert a PASS.
6. `vtx_advance` — advance the run only if the gate returned PASS.
7. `vtx_open_pr` — open a **Draft** PR (never merge or deploy). The spine refuses unless the gate is PASS for this exact run + SHA.

If any step fails, the SHA drifts after QA, or the gate does not pass, call `vtx_block` (or `vtx_escalate` for a human) with the reason. Never fabricate evidence, a verdict, or a passing gate.
