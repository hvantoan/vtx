# REVIEW.md — Code Review Criteria & Quality Gates

This document defines the review criteria applied by human reviewers and `/ak:code-review` agents.

---

## 1. Automated Acceptance Gates (Must Pass)

- [ ] `./infra/scripts/check-boundaries.sh` passes with zero violations.
- [ ] Exactly one `go.mod` at `apps/backend/go.mod`; NO `go.work` in repo.
- [ ] `go vet` and Go tests pass with race detector (`go test -v -race ./...`).
- [ ] TypeScript typecheck passes in all JS packages (`tsc --noEmit`).
- [ ] Next.js production build passes (`next build`).
- [ ] OpenAPI 3.1 specification lints cleanly without schema errors.
- [ ] Secret scanner detects no high-entropy keys or tokens.

---

## 2. Architectural Integrity

- [ ] **Deterministic Temporal Workflows**: No calls to `time.Now()`, `rand.Int()`, or direct network/database/filesystem calls inside workflow functions.
- [ ] **Subprocess Cleanup**: Agent supervisor uses Unix process groups (`syscall.Setpgid`) and propagates termination to prevent orphaned grandchild processes.
- [ ] **Web Isolation**: `apps/web` has no imports from backend or agent-runtime and does not mount or reference `/var/run/docker.sock`.
- [ ] **Constant-Time Auth**: Token comparisons use `crypto/subtle.ConstantTimeCompare`.
- [ ] **SSE Streaming Timers**: HTTP server `WriteTimeout` is 0 to prevent closing persistent SSE connections.

---

## 3. Code Quality Standards

- [ ] Go standard library `log/slog` used for structured JSON logging.
- [ ] TypeScript code avoids `: any` or `as any`.
- [ ] Test files avoid arbitrary wall-clock timer sleeps (`ts-no-test-timers`).
- [ ] Idempotent database migrations with Goose.
