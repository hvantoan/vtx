package execcmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessRunnerWorktreeAndBoundedCapture(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\t@printf 'abcdefghij'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := (ProcessRunner{CaptureLimit: 4}).Run(context.Background(), CmdSpec{Name: "make-test", Argv: []string{"make", "-s", "-C", dir, "test"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("abcdefghij")))
	if r.ExitCode != 0 || !r.Truncated || string(r.Output) != "abcd" || string(r.Tail) != "abcdefghij" || r.DigestHex != digest {
		t.Fatalf("%+v", r)
	}
}
func TestSuitePlanAllowlist(t *testing.T) {
	for _, s := range SuitePlan("/run/;echo injected") {
		if s.Argv[0] != "make" || s.Argv[1] != "-C" || s.Argv[2] != "/run/;echo injected" || strings.Contains(s.Argv[3], " ") {
			t.Fatal(s)
		}
	}
}
func TestCaptureBufferExactLimitNotTruncated(t *testing.T) {
	b := captureBuffer{limit: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if b.Truncated() {
		t.Fatal("exact capture marked truncated")
	}
	if _, err := b.Write([]byte("5")); err != nil {
		t.Fatal(err)
	}
	if !b.Truncated() {
		t.Fatal("overflow not marked")
	}
}
