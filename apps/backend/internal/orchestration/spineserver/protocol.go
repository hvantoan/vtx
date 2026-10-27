// Package spineserver exposes VTX's existing orchestration services to the
// Pi orchestrator extension over a unix-domain socket (VTX-ORCH Phase 1).
//
// The spine is the authority: it owns state transitions, lease fencing, gate
// decisions, and PR creation. The agent only asks the spine to run a tool; it
// never forges evidence or decides a gate. Every handler is a thin wrapper over
// a service that already exists — this package adds transport, auth, and
// fail-closed routing, not business logic.
package spineserver

import "encoding/json"

// Tool names the orchestrator extension may invoke. They mirror the vtx_* tools
// registered by the extension (docs/design/vtx-orchestrator-extension.md §3.2).
const (
	ToolIntake     = "intake"
	ToolVerify     = "verify"
	ToolGate       = "gate"
	ToolQALaunch   = "qa_launch"
	ToolQAVerdict  = "qa_verdict"
	ToolOpenPR     = "open_pr"
	ToolAdvance    = "advance"
	ToolBlock      = "block"
	ToolRenewLease = "renew_lease"
)

// Error codes returned in Response.Error.Code. They are stable strings the
// extension switches on; the human message is never parsed.
const (
	ErrCodeUnauthenticated = "unauthenticated"
	ErrCodeUnknownTool     = "unknown_tool"
	ErrCodeToolUnavailable = "tool_unavailable"
	ErrCodeInvalidRequest  = "invalid_request"
	ErrCodeInvalidArgs     = "invalid_args"
	ErrCodeService         = "service_error"
)

// Request is one NDJSON line from the extension. RunID and LeaseEpoch are
// top-level so the spine can fence every mutating call without trusting nested
// args; Args carries the tool-specific command payload.
type Request struct {
	ID         string          `json:"id"`
	Token      string          `json:"token"`
	Tool       string          `json:"tool"`
	RunID      string          `json:"run_id"`
	LeaseEpoch int64           `json:"lease_epoch"`
	Args       json.RawMessage `json:"args,omitempty"`
}

// ResponseError is the machine-readable failure half of a Response.
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Response is one NDJSON line back to the extension. Result holds the service's
// structured output (evidence, decision, view) for the extension to sync.
type Response struct {
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Tool   string         `json:"tool"`
	Result any            `json:"result,omitempty"`
	Error  *ResponseError `json:"error,omitempty"`
}

func errResponse(id, tool, code, msg string) Response {
	return Response{ID: id, Tool: tool, OK: false, Error: &ResponseError{Code: code, Message: msg}}
}

func okResponse(id, tool string, result any) Response {
	return Response{ID: id, Tool: tool, OK: true, Result: result}
}
