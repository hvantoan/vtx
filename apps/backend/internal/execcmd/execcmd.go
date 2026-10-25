// Package execcmd is VTX-8's process boundary: every subprocess execution
// for verification goes through Runner, mirroring gitops' discipline. The
// suite allowlist is compile-time — no caller/agent string ever reaches exec.
package execcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CmdSpec is a fully-resolved, allowlisted command. No shell. Ever.
type CmdSpec struct {
	Name string   // suite name, e.g. "make-test"
	Argv []string // e.g. {"make", "-C", dir, "test"} — dir already canonicalized
	Dir  string   // cwd = canonical worktree path
	Env  []string // minimal allowlist: PATH, HOME, GOPATH/GOCACHE, pnpm vars
}

// Result is the outcome of one suite run.
type Result struct {
	ExitCode  int
	TimedOut  bool
	Output    []byte // captured combined stdout+stderr, bounded at MaxCapture
	Tail      []byte // last LogTailSize bytes of the stream
	Truncated bool   // true when the stream exceeded MaxCapture
	DigestHex string // sha256 of the captured output
	Duration  time.Duration
}

// Runner executes one spec under ctx deadline; the deadline maps to
// Result.TimedOut rather than an opaque ctx error.
type Runner interface {
	Run(ctx context.Context, spec CmdSpec) (Result, error)
}

// Bounds (vtx-8 §6): the inline tail never exceeds 64 KiB; the captured log
// is bounded at 32 MiB (the artifact file cap).
const (
	LogTailSize = 64 * 1024
	MaxCapture  = 32 * 1024 * 1024
)

// ProcessRunner is the production Runner (os/exec). Output is bounded:
// the full stream is hashed, the first MaxCapture bytes are kept for the
// artifact sink, and the last LogTailSize bytes for the inline tail.
type ProcessRunner struct {
	// EnvAllowlist is the set of parent-process env vars propagated to the
	// child. Nil uses envAllowlist.
	EnvAllowlist []string
	// CaptureLimit overrides MaxCapture (tests use small limits).
	CaptureLimit int
}

// envAllowlist is the minimal environment a make-based suite needs — and the
// only thing that ever crosses the process boundary (NFR-SEC-001).
var envAllowlist = []string{
	"PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS",
	"PNPM_HOME", "NPM_CONFIG_CACHE", "XDG_CACHE_HOME", "CI",
}

func (r ProcessRunner) Run(ctx context.Context, spec CmdSpec) (Result, error) {
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("execcmd: empty argv for suite %q", spec.Name)
	}
	cap := r.CaptureLimit
	if cap <= 0 {
		cap = MaxCapture
	}
	env := spec.Env
	if len(env) == 0 {
		allow := r.EnvAllowlist
		if len(allow) == 0 {
			allow = envAllowlist
		}
		env = allowlistedEnv(allow)
	}

	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = env
	// Bound the post-kill wait for the I/O copiers when the deadline fires:
	// CommandContext kills the process, and WaitDelay keeps a child holding
	// the pipes open from hanging Run forever.
	cmd.WaitDelay = 2 * time.Second

	var capture captureBuffer
	capture.limit = cap
	var tail tailBuffer
	tail.limit = LogTailSize
	hasher := sha256.New()
	cmd.Stdout = io.MultiWriter(&capture, &tail, hasher)
	cmd.Stderr = io.MultiWriter(&capture, &tail, hasher)

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Output:    capture.Bytes(),
		Tail:      tail.Bytes(),
		Truncated: capture.Truncated(),
		DigestHex: hex.EncodeToString(hasher.Sum(nil)),
		Duration:  time.Since(start),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if ctx.Err() != nil {
			res.TimedOut = true
		}
	default:
		if ctx.Err() != nil {
			res.TimedOut = true
			res.ExitCode = -1
			return res, nil
		}
		return res, fmt.Errorf("execcmd: spawn %s: %w", spec.Name, err)
	}
	return res, nil
}

// allowlistedEnv copies only the named variables from the process env.
func allowlistedEnv(names []string) []string {
	var out []string
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// captureBuffer keeps the first limit bytes of a stream and counts overflow.
type captureBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	if b.buf.Len() >= b.limit {
		b.truncated = true
	}
	return len(p), nil
}

func (b *captureBuffer) Bytes() []byte    { return b.buf.Bytes() }
func (b *captureBuffer) Truncated() bool  { return b.truncated }

// tailBuffer keeps the last limit bytes of a stream.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		// Keep the tail; copy to release the head's backing array.
		keep := make([]byte, b.limit)
		copy(keep, b.buf[len(b.buf)-b.limit:])
		b.buf = keep
	}
	return len(p), nil
}

func (b *tailBuffer) Bytes() []byte { return b.buf }

// Suite is one allowlisted make target.
type Suite struct {
	Name      string // verification_runs.suite
	Target    string // make target
	Mandatory bool   // all suites are mandatory in vtx-gate-policy v1
}

// SuitePlan is the ordered allowlist (vtx-8 D8-1): the repository's own make
// contract, rendered as argv with -C <worktree>. It lives in code so nothing
// user-supplied reaches exec.
func SuitePlan(worktreeDir string) []CmdSpec {
	suites := []Suite{
		{Name: "make-test-boundaries", Target: "test-boundaries", Mandatory: true},
		{Name: "make-vet", Target: "vet", Mandatory: true},
		{Name: "make-typecheck", Target: "typecheck", Mandatory: true},
		{Name: "make-test", Target: "test", Mandatory: true},
		{Name: "make-build", Target: "build", Mandatory: true},
	}
	out := make([]CmdSpec, 0, len(suites))
	for _, s := range suites {
		out = append(out, CmdSpec{
			Name: s.Name,
			Argv: []string{"make", "-C", worktreeDir, s.Target},
			Dir:  worktreeDir,
		})
	}
	return out
}

// Command renders the argv the way it is stored on the row — for audit, not
// for re-execution (the command string is never re-parsed).
func Command(spec CmdSpec) string { return strings.Join(spec.Argv, " ") }
