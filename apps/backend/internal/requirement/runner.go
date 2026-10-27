package requirement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/gitops"
)

// ProcessRunner is the baseline adapter for the VTX-8 execution port. Only
// configured make targets reach exec; neither Jira text nor agent argv can.
type ProcessRunner struct {
	Layout gitops.Layout
	Git    gitops.Git
}

func (r ProcessRunner) RunBaseline(ctx context.Context, w application.WorktreeView, command string) (SuiteResult, error) {
	argv := strings.Fields(command)
	if len(argv) != 2 || argv[0] != "make" {
		return SuiteResult{}, fmt.Errorf("baseline command must be an allowlisted make target")
	}
	switch argv[1] {
	case "test-boundaries", "vet", "typecheck", "test", "build":
	default:
		return SuiteResult{}, fmt.Errorf("baseline target not allowlisted")
	}
	path, err := gitops.CanonicalPath(r.Layout.WorktreeRoot, w.Path)
	if err != nil {
		return SuiteResult{}, err
	}
	sha, err := r.Git.HeadSHA(ctx, path)
	if err != nil {
		return SuiteResult{}, fmt.Errorf("baseline HEAD: %w", err)
	}
	if w.BaseSHA == nil || sha != *w.BaseSHA {
		return SuiteResult{}, fmt.Errorf("baseline HEAD differs from base_sha")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1])
	cmd.Dir = path
	for _, key := range []string{"PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE", "PNPM_HOME"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	configureBaselineProcess(cmd)
	capture := &boundedCapture{digest: sha256.New()}
	cmd.Stdout = capture
	cmd.Stderr = capture
	start := time.Now()
	err = cmd.Run()
	result := SuiteResult{ExitCode: 0, Output: capture.tail, Duration: time.Since(start), Digest: hex.EncodeToString(capture.digest.Sum(nil)), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("execute baseline: %w", err)
	}
	return result, nil
}

type boundedCapture struct {
	mu     sync.Mutex
	digest hash.Hash
	tail   []byte
}

func (c *boundedCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.digest.Write(p)
	if err != nil {
		return n, err
	}
	const max = 64 * 1024
	if len(p) >= max {
		c.tail = append(c.tail[:0], p[len(p)-max:]...)
	} else {
		extra := len(c.tail) + len(p) - max
		if extra > 0 {
			copy(c.tail, c.tail[extra:])
			c.tail = c.tail[:len(c.tail)-extra]
		}
		c.tail = append(c.tail, p...)
	}
	return len(p), nil
}
