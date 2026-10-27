package spineserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/gate"
	"github.com/vtx/platform/internal/qa"
)

const testToken = "spine-test-token"

// --- fakes -----------------------------------------------------------------

type fakeVerifier struct {
	got     application.VerifyCommand
	out     application.VerifyOutcome
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeVerifier) VerifyRun(_ context.Context, cmd application.VerifyCommand) (application.VerifyOutcome, error) {
	f.got = cmd
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	return f.out, f.err
}

type fakeGate struct {
	gotRun   string
	gotEpoch int64
	out      gate.Decision
	err      error
}

func (f *fakeGate) Evaluate(_ context.Context, runID string, epoch int64) (gate.Decision, error) {
	f.gotRun, f.gotEpoch = runID, epoch
	return f.out, f.err
}

type fakeQA struct {
	launchGot qa.LaunchCommand
	submitGot qa.SubmitCommand
	session   qa.Session
	verdict   qa.VerdictRecord
	launchErr error
	submitErr error
}

func (f *fakeQA) LaunchSession(_ context.Context, cmd qa.LaunchCommand) (qa.Session, error) {
	f.launchGot = cmd
	return f.session, f.launchErr
}
func (f *fakeQA) SubmitVerdict(_ context.Context, cmd qa.SubmitCommand) (qa.VerdictRecord, error) {
	f.submitGot = cmd
	return f.verdict, f.submitErr
}

type fakePR struct {
	got application.OpenPRCommand
	out application.PullRequestView
	err error
}

func (f *fakePR) OpenDraftPR(_ context.Context, cmd application.OpenPRCommand) (application.PullRequestView, error) {
	f.got = cmd
	return f.out, f.err
}

type fakeTransitions struct {
	got application.TransitionCommand
	out application.RunSnapshot
	err error
}

func (f *fakeTransitions) TransitionRun(_ context.Context, cmd application.TransitionCommand) (application.RunSnapshot, error) {
	f.got = cmd
	return f.out, f.err
}

type fakeIntake struct {
	got application.PollIntakeRequest
	out application.PollIntakeResult
	err error
}

func (f *fakeIntake) Intake(_ context.Context, req application.PollIntakeRequest) (application.PollIntakeResult, error) {
	f.got = req
	return f.out, f.err
}

type fakeLease struct {
	gotRun   string
	gotEpoch int64
	expiry   time.Time
	err      error
}

func (f *fakeLease) RenewLease(_ context.Context, runID string, epoch int64) (time.Time, error) {
	f.gotRun, f.gotEpoch = runID, epoch
	return f.expiry, f.err
}

func newTestServer(t *testing.T, ports Ports) *Server {
	t.Helper()
	s, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "s.sock"), AuthToken: testToken, LeaseOwner: "orch-owner"}, ports, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func call(t *testing.T, s *Server, req Request) Response {
	t.Helper()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	return s.Handle(context.Background(), line)
}

// --- auth & routing --------------------------------------------------------

func TestNewRejectsEmptyToken(t *testing.T) {
	if _, err := New(Config{SocketPath: "/tmp/x.sock"}, Ports{}, nil); err == nil {
		t.Fatal("expected error for empty auth token (fail closed)")
	}
}

func TestHandleRejectsBadToken(t *testing.T) {
	s := newTestServer(t, Ports{})
	resp := call(t, s, Request{ID: "1", Token: "wrong", Tool: ToolGate, RunID: "run-1"})
	if resp.OK || resp.Error == nil || resp.Error.Code != ErrCodeUnauthenticated {
		t.Fatalf("expected unauthenticated, got %+v", resp)
	}
}

func TestHandleRequiresRunID(t *testing.T) {
	s := newTestServer(t, Ports{})
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolGate})
	if resp.OK || resp.Error.Code != ErrCodeInvalidRequest {
		t.Fatalf("expected invalid_request, got %+v", resp)
	}
}

func TestIntakeIsNotRunScoped(t *testing.T) {
	in := &fakeIntake{out: application.PollIntakeResult{Disposition: application.PollDispositionAccepted, IntentID: "intent-1"}}
	s := newTestServer(t, Ports{Intake: in})
	args := json.RawMessage(`{"issue_id":"10014","issue_key":"VTX-5","version":"v1"}`)
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolIntake, Args: args})
	if !resp.OK {
		t.Fatalf("expected ok without run_id, got %+v", resp.Error)
	}
	if in.got.IssueID != "10014" || in.got.IssueKey != "VTX-5" || in.got.Version != "v1" {
		t.Fatalf("intake args not decoded: %+v", in.got)
	}
}

func TestIntakeStillRequiresAuth(t *testing.T) {
	s := newTestServer(t, Ports{Intake: &fakeIntake{}})
	resp := call(t, s, Request{ID: "1", Token: "wrong", Tool: ToolIntake})
	if resp.OK || resp.Error.Code != ErrCodeUnauthenticated {
		t.Fatalf("expected unauthenticated, got %+v", resp)
	}
}

func TestHandleUnknownTool(t *testing.T) {
	s := newTestServer(t, Ports{})
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: "nope", RunID: "run-1"})
	if resp.OK || resp.Error.Code != ErrCodeUnknownTool {
		t.Fatalf("expected unknown_tool, got %+v", resp)
	}
}

func TestNilPortIsToolUnavailable(t *testing.T) {
	s := newTestServer(t, Ports{}) // no ports wired
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolVerify, RunID: "run-1", LeaseEpoch: 3})
	if resp.OK || resp.Error.Code != ErrCodeToolUnavailable {
		t.Fatalf("expected tool_unavailable, got %+v", resp)
	}
}

func TestHandleInvalidJSON(t *testing.T) {
	s := newTestServer(t, Ports{})
	resp := s.Handle(context.Background(), []byte("{not json"))
	if resp.OK || resp.Error.Code != ErrCodeInvalidRequest {
		t.Fatalf("expected invalid_request, got %+v", resp)
	}
}

// --- tool behavior ---------------------------------------------------------

func TestVerifyFenceDefaultsFromRequest(t *testing.T) {
	v := &fakeVerifier{out: application.VerifyOutcome{AllPassed: true, Verdict: "passed", SHA: "abc"}}
	s := newTestServer(t, Ports{Verifier: v})
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolVerify, RunID: "run-9", LeaseEpoch: 7})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if v.got.RunID != "run-9" || v.got.LeaseEpoch != 7 || v.got.LeaseOwner != "orch-owner" {
		t.Fatalf("fence defaults not applied: %+v", v.got)
	}
}

func TestVerifyDecodesArgs(t *testing.T) {
	v := &fakeVerifier{out: application.VerifyOutcome{AllPassed: true}}
	s := newTestServer(t, Ports{Verifier: v})
	args := json.RawMessage(`{"task_id":"VTX-9","attempt":4,"run_id":"evil"}`)
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolVerify, RunID: "run-9", LeaseEpoch: 7, Args: args})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if v.got.TaskID != "VTX-9" || v.got.Attempt != 4 {
		t.Fatalf("task_id/attempt not decoded: %+v", v.got)
	}
	if v.got.RunID != "run-9" {
		t.Fatalf("run_id must come from the fenced request: %+v", v.got)
	}
}

func TestGateIgnoresAgentVerdict(t *testing.T) {
	g := &fakeGate{out: gate.Decision{Decision: "pass"}}
	s := newTestServer(t, Ports{Gate: g})
	// The agent tries to smuggle a verdict/decision into args; the spine must
	// pass only run_id + lease_epoch to Evaluate (FR-QA-002: gate not LLM-alone).
	args := json.RawMessage(`{"lease_epoch":5,"decision":"pass","verdict":"pass"}`)
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolGate, RunID: "run-1", LeaseEpoch: 9, Args: args})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if g.gotRun != "run-1" || g.gotEpoch != 5 {
		t.Fatalf("gate args wrong: run=%q epoch=%d", g.gotRun, g.gotEpoch)
	}
}

func TestAdvanceFixesActorAndType(t *testing.T) {
	tr := &fakeTransitions{out: application.RunSnapshot{RunID: "run-1", State: domain.StateVerifying, Version: 7}}
	s := newTestServer(t, Ports{Transitions: tr})
	args := json.RawMessage(`{"expected_version":4,"reason":"coding done","metadata":{"sha":"abc"}}`)
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolAdvance, RunID: "run-1", LeaseEpoch: 2, Args: args})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if tr.got.Action != domain.ActionAdvance || tr.got.Actor != domain.ActorOrchestrator || tr.got.ActorType != domain.ActorTypeSystem {
		t.Fatalf("actor/type not fixed by spine: %+v", tr.got)
	}
	if tr.got.ExpectedVersion != 4 || tr.got.ExpectedLeaseEpoch != 2 || string(tr.got.Metadata) == "" {
		t.Fatalf("CAS fields not propagated: %+v", tr.got)
	}
	// The extension fences its next CAS on this shape; Go field names would
	// leave it stuck on the launch-time version after the first advance.
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version":7`) || !strings.Contains(string(raw), `"state":"verifying"`) {
		t.Fatalf("snapshot not encoded as the tool protocol: %s", raw)
	}
	if strings.Contains(string(raw), `"Version"`) {
		t.Fatalf("application field names leaked into the protocol: %s", raw)
	}
}

func TestBlockUsesPolicyActor(t *testing.T) {
	tr := &fakeTransitions{}
	s := newTestServer(t, Ports{Transitions: tr})
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolBlock, RunID: "run-1", Args: json.RawMessage(`{"reason":"budget"}`)})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if tr.got.Action != domain.ActionBlock || tr.got.Actor != domain.ActorPolicy {
		t.Fatalf("block must run as policy actor: %+v", tr.got)
	}
}

func TestQAVerdictForcesIdentity(t *testing.T) {
	q := &fakeQA{verdict: qa.VerdictRecord{Verdict: domain.QAVerdictPass}}
	s := newTestServer(t, Ports{QA: q})
	// Agent tries to claim a different identity; spine must overwrite it.
	args := json.RawMessage(`{"commit_sha":"abc","verdict":"pass","qa_agent_identity":"hacker"}`)
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolQAVerdict, RunID: "run-1", LeaseEpoch: 1, Args: args})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if q.submitGot.QAIdentity != "qa-agent" || q.submitGot.RunID != "run-1" {
		t.Fatalf("identity not forced server-side: %+v", q.submitGot)
	}
}

func TestOpenPROverridesRunID(t *testing.T) {
	p := &fakePR{out: application.PullRequestView{RunID: "run-1", Draft: true}}
	s := newTestServer(t, Ports{PRs: p})
	args := json.RawMessage(`{"run_id":"evil","title":"t","body":"b","expected_version":3}`)
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolOpenPR, RunID: "run-1", LeaseEpoch: 8, Args: args})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if p.got.RunID != "run-1" || p.got.ExpectedLeaseEpoch != 8 || p.got.ExpectedVersion != 3 {
		t.Fatalf("run_id must come from the fenced request, not args: %+v", p.got)
	}
}

func TestRenewLease(t *testing.T) {
	l := &fakeLease{expiry: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := newTestServer(t, Ports{LeaseRenewer: l})
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolRenewLease, RunID: "run-1", LeaseEpoch: 6})
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp.Error)
	}
	if l.gotRun != "run-1" || l.gotEpoch != 6 {
		t.Fatalf("renew args wrong: %+v", l)
	}
}

func TestServiceErrorPropagates(t *testing.T) {
	v := &fakeVerifier{err: errors.New("boom")}
	s := newTestServer(t, Ports{Verifier: v})
	resp := call(t, s, Request{ID: "1", Token: testToken, Tool: ToolVerify, RunID: "run-1"})
	if resp.OK || resp.Error.Code != ErrCodeService || resp.Error.Message != "boom" {
		t.Fatalf("expected service_error boom, got %+v", resp)
	}
}

// --- transport over a real unix socket -------------------------------------

// TestSlowToolDoesNotStarveHeartbeat proves one connection multiplexes: a
// verification that runs for minutes must not delay a lease renewal queued
// behind it, or the lease expires while the suite is still running.
func TestSlowToolDoesNotStarveHeartbeat(t *testing.T) {
	v := &fakeVerifier{
		out:     application.VerifyOutcome{AllPassed: true},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	l := &fakeLease{expiry: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sock := filepath.Join(t.TempDir(), "spine.sock")
	s, err := New(Config{SocketPath: sock, AuthToken: testToken}, Ports{Verifier: v, LeaseRenewer: l}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx) }()

	conn, err := dialWhenReady(sock)
	if err != nil {
		t.Fatalf("dial spine socket: %v", err)
	}
	defer conn.Close()
	responses := make(chan Response, 4)
	go func() {
		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadBytes('\n')
			if readErr != nil {
				return
			}
			var resp Response
			if json.Unmarshal(line, &resp) == nil {
				responses <- resp
			}
		}
	}()

	verifyReq, _ := json.Marshal(Request{ID: "slow", Token: testToken, Tool: ToolVerify, RunID: "run-1", LeaseEpoch: 3})
	if _, err := conn.Write(append(verifyReq, '\n')); err != nil {
		t.Fatalf("write verify: %v", err)
	}
	select {
	case <-v.started:
	case <-time.After(3 * time.Second):
		t.Fatal("verify never reached the service")
	}

	renewReq, _ := json.Marshal(Request{ID: "beat", Token: testToken, Tool: ToolRenewLease, RunID: "run-1", LeaseEpoch: 3})
	if _, err := conn.Write(append(renewReq, '\n')); err != nil {
		t.Fatalf("write renew: %v", err)
	}
	select {
	case resp := <-responses:
		if resp.ID != "beat" || !resp.OK {
			t.Fatalf("expected the renew response while verify is still running, got %+v", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("renew_lease starved behind the running verify")
	}

	close(v.release)
	select {
	case resp := <-responses:
		if resp.ID != "slow" || !resp.OK {
			t.Fatalf("expected the verify response, got %+v", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("verify response never arrived")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop after cancel")
	}
}

// dialWhenReady retries until the server has bound its socket.
func dialWhenReady(sock string) (net.Conn, error) {
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

func TestServeOverUnixSocket(t *testing.T) {
	g := &fakeGate{out: gate.Decision{Decision: "pass", RunID: "run-1"}}
	sock := filepath.Join(t.TempDir(), "spine.sock")
	s, err := New(Config{SocketPath: sock, AuthToken: testToken}, Ports{Gate: g}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx) }()

	// Wait for the socket to appear.
	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", sock)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial spine socket: %v", err)
	}
	defer conn.Close()

	req, _ := json.Marshal(Request{ID: "c1", Token: testToken, Tool: ToolGate, RunID: "run-1", LeaseEpoch: 2})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal resp: %v (%q)", err, line)
	}
	if !resp.OK || resp.ID != "c1" || resp.Tool != ToolGate {
		t.Fatalf("unexpected response: %+v", resp)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop after cancel")
	}
}
