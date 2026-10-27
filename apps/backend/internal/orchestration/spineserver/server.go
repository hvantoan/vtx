package spineserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxRequestBytes bounds one NDJSON request line. Tool args are small commands;
// a larger line is a protocol error, not something to buffer unboundedly.
const maxRequestBytes = 1 << 20 // 1 MiB

// Config configures a Server. SocketPath and AuthToken are required; the rest
// default to safe values.
type Config struct {
	// SocketPath is the unix-domain socket the extension connects to. The
	// directory must exist; a stale socket file at this path is removed.
	SocketPath string
	// AuthToken is compared in constant time against Request.Token. Empty
	// disables the server (fail closed) — an unauthenticated spine is never
	// started.
	AuthToken string
	// LeaseOwner is the identity written on transitions/renewals this spine
	// makes on the orchestrator's behalf.
	LeaseOwner string
	// ReadTimeout bounds a single request read so a stuck client cannot pin a
	// connection goroutine forever. It must stay comfortably above the
	// orchestrator's 30s lease-renew heartbeat: an idle drop would otherwise
	// race the heartbeat and permanently disconnect the session.
	// Zero defaults to 90s.
	ReadTimeout time.Duration
	// MaxSocketBacklog is unused today; kept explicit for future tuning.
}

func (c Config) withDefaults() Config {
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 90 * time.Second
	}
	if c.LeaseOwner == "" {
		c.LeaseOwner = "orchestrator"
	}
	return c
}

// Server routes authenticated tool requests to the injected service ports.
type Server struct {
	cfg    Config
	log    *slog.Logger
	ports  Ports
	listen net.Listener
}

// Ports bundles the optional service dependencies. Any nil field disables its
// tool with ErrCodeToolUnavailable rather than failing startup.
type Ports struct {
	Intake       Intake
	Verifier     Verifier
	Gate         GateEvaluator
	QA           QA
	PRs          PROpener
	Transitions  Transitioner
	LeaseRenewer LeaseRenewer
}

// New builds a Server. It does not listen; call Serve. An empty AuthToken or
// SocketPath is a configuration error (fail closed).
func New(cfg Config, ports Ports, logger *slog.Logger) (*Server, error) {
	if cfg.AuthToken == "" {
		return nil, errors.New("spineserver: AuthToken is required (refusing to start unauthenticated)")
	}
	if cfg.SocketPath == "" {
		return nil, errors.New("spineserver: SocketPath is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg.withDefaults(), ports: ports, log: logger}, nil
}

// Serve listens on the unix socket and handles connections until ctx is
// cancelled or the listener fails. It removes a stale socket file first and
// cleans up the socket on return.
func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o700); err != nil {
		return fmt.Errorf("spineserver: create socket dir: %w", err)
	}
	// A leftover socket from a crashed process makes bind fail with EADDRINUSE.
	// It is safe to remove because nothing is listening once the process is gone.
	if _, err := os.Stat(s.cfg.SocketPath); err == nil {
		if probe, dialErr := net.Dial("unix", s.cfg.SocketPath); dialErr == nil {
			_ = probe.Close()
			return fmt.Errorf("spineserver: socket %s is already in use by a live process", s.cfg.SocketPath)
		}
		if err := os.Remove(s.cfg.SocketPath); err != nil {
			return fmt.Errorf("spineserver: remove stale socket: %w", err)
		}
	}

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("spineserver: listen: %w", err)
	}
	s.listen = ln
	// Owner-only: the socket is a credential boundary.
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("spineserver: chmod socket: %w", err)
	}
	s.log.Info("spine tool server listening", slog.String("socket", s.cfg.SocketPath))

	// Close the listener when ctx ends so Accept unblocks.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			s.log.Error("spine accept failed", "error", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}

	_ = os.Remove(s.cfg.SocketPath)
	s.log.Info("spine tool server stopped")
	if ctx.Err() != nil {
		return nil
	}
	return nil
}

// Addr exposes the bound listener address (tests use it to dial).
func (s *Server) Addr() net.Addr {
	if s.listen == nil {
		return nil
	}
	return s.listen.Addr()
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	var writeMu sync.Mutex
	write := func(resp Response) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		return s.writeLine(writer, resp)
	}
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		line, err := reader.ReadBytes('\n')
		if len(line) > maxRequestBytes {
			write(errResponse("", "", ErrCodeInvalidRequest, "request exceeds size limit"))
			return
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("spine connection read ended", "error", err)
			}
			return
		}
		// Each request runs on its own goroutine: a synchronous tool that takes
		// minutes (verification) must not queue the lease heartbeat behind it on
		// the same connection, or the lease expires mid-suite. Responses are
		// serialized and correlated by id.
		go func(line []byte) {
			if !write(s.Handle(ctx, line)) {
				_ = conn.Close()
			}
		}(line)
	}
}

func (s *Server) writeLine(w *bufio.Writer, resp Response) bool {
	buf, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("spine response marshal failed", "error", err)
		return true // keep the connection; the client sees no line and retries
	}
	buf = append(buf, '\n')
	if _, err := w.Write(buf); err != nil {
		return false
	}
	return w.Flush() == nil
}

// Handle parses, authenticates, and routes one request line. It never panics on
// malformed input and always returns a Response with the request id when it can
// be parsed. Exported for unit tests that drive the router without a socket.
func (s *Server) Handle(ctx context.Context, line []byte) Response {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return errResponse("", "", ErrCodeInvalidRequest, "invalid JSON request")
	}
	// Authenticate before anything else, and before logging the tool, so an
	// unauthenticated caller cannot probe which tools exist.
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.cfg.AuthToken)) != 1 {
		s.log.Warn("spine request rejected: bad token", slog.String("id", req.ID))
		return errResponse(req.ID, req.Tool, ErrCodeUnauthenticated, "invalid or missing token")
	}
	// Every state-mutating tool is run-scoped and fenced by lease epoch; reject
	// a missing run_id before routing so no handler can act on an ambiguous
	// target. Intake is the deliberate exception: it creates the run's intent,
	// so no run exists yet. It is authenticated like every other tool.
	if req.RunID == "" && req.Tool != ToolIntake {
		return errResponse(req.ID, req.Tool, ErrCodeInvalidRequest, "run_id is required")
	}
	s.log.Info("spine tool call",
		slog.String("id", req.ID),
		slog.String("tool", req.Tool),
		slog.String("run_id", req.RunID),
		slog.Int64("lease_epoch", req.LeaseEpoch),
	)
	return s.route(ctx, req)
}
