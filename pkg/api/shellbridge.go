package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"

	"shpyrd/pkg/authz"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/kexec"
)

// The WebSocket half of the web terminal (RFC-0026). Binary frames carry
// terminal bytes in both directions; text frames carry JSON control, so a
// resize needs no second connection and an exit code can be reported after
// the bytes stop.

// shellIdleTimeout ends a session nobody is typing into. The timer follows
// client input only: a process that keeps printing must not hold a terminal
// open for someone who has walked away.
const shellIdleTimeout = 30 * time.Minute

// shellPingInterval keeps a quiet session alive. ingress-nginx fronts the
// dashboard and reaps an idle upstream after its default 60s
// proxy_read_timeout, and a browser cannot send pings from JavaScript, so
// without pings from this side a live terminal nobody is typing into would die
// in about a minute and shellIdleTimeout above could never be reached.
const shellPingInterval = 30 * time.Second

// shellReadLimit caps an inbound frame. See the call site in appShell: it
// bounds what one client can make this single-replica server allocate.
const shellReadLimit = 1 << 20

// shellCandidates is the fallback order, the same as `shpyrd shell`
// (internal/cli/shell.go). The launcher comes first so buildpack images get
// their environment; the order is the contract between the two, not the code.
var shellCandidates = [][]string{
	{cnbLauncher, "--", "bash"},
	{"bash"},
	{cnbLauncher, "--", "sh"},
	{"sh"},
}

// cnbLauncher loads a buildpack image's environment before the shell.
const cnbLauncher = "/cnb/lifecycle/launcher"

// errNoShell is what an image with neither bash nor sh produces.
var errNoShell = errors.New("no usable shell in the image")

type execStreamFunc func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error

type probeShellFunc func(ctx context.Context, namespace, pod, container string) ([]string, error)

// shellControl is a text frame. Fields not relevant to a type stay unset.
type shellControl struct {
	Type     string `json:"type"`
	Instance string `json:"instance,omitempty"`
	Shell    string `json:"shell,omitempty"`
	Cols     uint16 `json:"cols,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
	Code     *int   `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// appShell bridges a browser terminal to a pod. It authenticates by ticket
// alone: a browser cannot set headers on a WebSocket, and `shpyrd cluster
// dashboard` signs in with a token in localStorage, so there may be no
// session cookie to lean on either.
func (s *Server) appShell(c *gin.Context) {
	if !sameOrigin(c.Request) {
		abort(c, http.StatusForbidden, errors.New("cross-origin WebSocket refused"))
		return
	}
	t, err := s.execTickets.redeem(c.Query("ticket"))
	if err != nil {
		abort(c, http.StatusForbidden, err)
		return
	}
	slug := c.Param("slug")
	if t.Project != slug || t.Instance != c.Query("instance") {
		abort(c, http.StatusForbidden, errors.New("the ticket was issued for another project or instance"))
		return
	}
	// The identity comes from the ticket, but never the authorisation: roles
	// are resolved again so a grant revoked inside those 30 seconds still
	// takes effect.
	ext.SetIdentity(c, t.Identity)
	roles, err := s.authz.Roles(c.Request.Context(), t.Identity)
	if err != nil {
		abort(c, http.StatusBadGateway, fmt.Errorf("resolve roles: %w", err))
		return
	}
	if !roles.Can(authz.ProjectExec, slug) {
		abort(c, http.StatusForbidden, denial(roles, authz.ProjectExec, slug))
		return
	}
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	instances, err := s.instancesOf(c, app.Namespace, app.Name)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	pod := ""
	for _, i := range instances {
		if i.Name == t.Instance {
			pod = i.Pod
		}
	}
	if pod == "" {
		// Routine during a rolling deploy: the instance was there when the
		// selector loaded and is gone now.
		abort(c, http.StatusNotFound, fmt.Errorf("instance %q is no longer running; running: %s", t.Instance, instanceList(instances)))
		return
	}
	actor := actorKey(t.Identity)
	if !s.shells.claim(actor, app.Name) {
		abort(c, http.StatusConflict, errors.New("you already have a shell open on this project; close it first"))
		return
	}
	defer s.shells.release(actor, app.Name)

	up := websocket.Upgrader{CheckOrigin: sameOrigin}
	conn, err := up.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade has already written its own response.
		s.log.Warn("shell: upgrade failed", "project", app.Name, "error", err)
		return
	}
	defer conn.Close()
	// Gorilla reads an unlimited frame by default, so one client declaring a
	// multi-gigabyte payload would make ReadMessage grow a buffer until the
	// process died. The cap is not about the terminal — a keystroke stream
	// needs nothing close to a megabyte — it is about the blast radius: this
	// API server runs at replicas: 1 and holds the ticket store, the shell
	// registry and every other tenant's dashboard, and this route is reachable
	// by anyone holding project.exec on a single project of their own.
	conn.SetReadLimit(shellReadLimit)
	s.runShell(c, conn, app.Namespace, pod, t.Instance, app.Name)
}

// runShell resolves the shell, then pipes the socket to the pod until one end
// stops.
func (s *Server) runShell(c *gin.Context, conn *websocket.Conn, namespace, pod, instance, project string) {
	w := &wsConn{c: conn}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	command, err := s.probeShell(ctx, namespace, pod, appContainer)
	if err != nil {
		_ = w.control(shellControl{Type: "error", Message: err.Error()})
		_ = w.close(websocket.CloseInternalServerErr, err.Error())
		// Audited even though no terminal opened: probing has by now run up to
		// four pods/exec calls against a running pod, and minting the ticket
		// records nothing, so without this the whole attempt — who, which
		// instance, and an RBAC or apiserver failure behind those four execs —
		// would leave no trace at all.
		s.audit(c, project, "shell.close", instance, err.Error())
		return
	}
	shell := command[len(command)-1]
	_ = w.control(shellControl{Type: "open", Instance: instance, Shell: shell})
	s.audit(c, project, "shell.open", instance, shell)

	// stdin is a pipe so the reader goroutine can hand bytes to the exec
	// stream, and closing it is how a closed socket ends the remote command.
	pr, pw := io.Pipe()
	// Closing the read half releases the reader goroutine if it is parked in
	// Write. A process can stop reading stdin long before it exits (`less`
	// after 'q'), and an io.Pipe write blocks until someone reads: once the
	// stream is done nobody ever will, and a closed socket does not reach a
	// goroutine blocked in Write rather than in ReadMessage. Without this the
	// goroutine and its pipe would leak for the life of the server.
	defer pr.Close()
	sizes := make(chan remotecommand.TerminalSize, 4)
	idle := time.NewTimer(s.shellIdle)
	defer idle.Stop()

	// The reader goroutine is the only owner of sizes: it is the only sender,
	// so it is the only safe closer. `defer close(sizes)` below must stay the
	// single close in this file — do not add a second one after execStream
	// returns, and do not reach for a sync.Once to make one safe.
	//
	// The stakes, since no test can catch a regression here: a user dragging
	// their browser window as their process exits would send a resize into a
	// channel another path had already closed. That panics, and a panic in a
	// bare goroutine is not recovered by gin's Recovery — it would take down
	// the API server, and with it every other tenant's dashboard. The window is
	// narrow enough that it would pass review and tests and fail in production.
	//
	// The reader always exits: it parks in either ReadMessage, which the
	// handler's deferred conn.Close unblocks, or the stdin Write, which the
	// deferred pr.Close above unblocks.
	go func() {
		defer pw.Close()
		defer close(sizes)
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			idle.Reset(s.shellIdle)
			switch typ {
			case websocket.BinaryMessage, websocket.TextMessage:
				if typ == websocket.TextMessage {
					var ctl shellControl
					// Malformed control frames are ignored: a buggy client
					// must not be able to kill someone's session.
					if err := json.Unmarshal(data, &ctl); err == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
						select {
						case sizes <- remotecommand.TerminalSize{Width: ctl.Cols, Height: ctl.Rows}:
						default: // a resize storm need not be buffered
						}
					}
					continue
				}
				if _, err := pw.Write(data); err != nil {
					return
				}
			}
		}
	}()

	// The idle timer cancels the session; it is reset by client input only.
	// idled is closed before cancel so the exit path below can tell an idle
	// reap from the request context going away under a server shutdown — the
	// browser is told which one it was, and the trail should agree.
	idled := make(chan struct{})
	go func() {
		select {
		case <-idle.C:
			_ = w.control(shellControl{Type: "error", Message: fmt.Sprintf("shell closed after %s idle", s.shellIdle)})
			close(idled)
			cancel()
		case <-ctx.Done():
		}
	}()

	// Server pings keep a proxy from reaping a live but quiet session; see
	// shellPingInterval. A ping deliberately does not touch the idle timer: a
	// keepalive we sent ourselves is not client input and says nothing about
	// whether anyone is still at the keyboard, so letting it reset the timer
	// would hold a terminal open forever for someone who walked away. Pongs
	// are handled inside gorilla and never surface in the reader loop, so they
	// cannot reset it either.
	pings := time.NewTicker(s.shellPing)
	defer pings.Stop()
	go func() {
		for {
			select {
			case <-pings.C:
				_ = w.ping()
			case <-ctx.Done(): // always fires: runShell defers cancel
				return
			}
		}
	}()

	err = s.execStream(ctx, namespace, pod, appContainer, command, pr, w, sizes)

	// Only an error carrying a remote status is an exit. Reporting anything
	// else as one would tell the browser the command finished and write a
	// status into the audit trail that never happened, leaving an operator
	// unable to tell a user typing `exit 1` from this server losing the
	// cluster — and discarding the only description of what actually broke.
	var ee *kexec.ExitError
	var detail string
	switch {
	// Cancellation is checked before the error, not after: we ended this
	// session, so that is what happened, whatever the stream happened to
	// return on its way out. A stream that returns nil just as the idle timer
	// reaps it must not be recorded as a clean exit the user chose.
	case ctx.Err() != nil:
		detail = "closed: server shutting down"
		select {
		case <-idled:
			detail = fmt.Sprintf("closed: idle for %s", s.shellIdle)
		default:
		}
		_ = w.close(websocket.CloseNormalClosure, "closed")
	case err == nil:
		code := 0
		detail = "exit 0"
		_ = w.control(shellControl{Type: "exit", Code: &code})
		_ = w.close(websocket.CloseNormalClosure, detail)
	case errors.As(kexec.RemoteExit(err), &ee):
		detail = fmt.Sprintf("exit %d", ee.Code)
		_ = w.control(shellControl{Type: "exit", Code: &ee.Code})
		_ = w.close(websocket.CloseNormalClosure, detail)
	default:
		detail = err.Error()
		_ = w.control(shellControl{Type: "error", Message: detail})
		_ = w.close(websocket.CloseInternalServerErr, detail)
	}
	s.audit(c, project, "shell.close", instance, detail)
}

// streamExec is the real bridge: an exec stream with a TTY.
func (s *Server) streamExec(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
	u := kexec.ExecURL(s.kube, namespace, pod, container, command, true)
	// With a TTY there is one stream: stderr is folded into stdout.
	return kexec.StreamIO(ctx, s.kube, u, true, stdin, stdout, stdout, sizes)
}

// resolveShell reports the first candidate the image has. Each probe is a
// throwaway non-TTY exec that exits at once, so a missing shell never writes
// anything into the terminal the user is about to see.
func (s *Server) resolveShell(ctx context.Context, namespace, pod, container string) ([]string, error) {
	var lastErr error
	for _, cand := range shellCandidates {
		probe := append(append([]string{}, cand...), "-c", "exit 0")
		if _, err := kexec.Run(ctx, s.kube, namespace, pod, container, probe); err == nil {
			return cand, nil
		} else if !kexec.IsNotFound(err) {
			return nil, err
		} else {
			lastErr = err
		}
	}
	return nil, fmt.Errorf("%w: %v", errNoShell, lastErr)
}

// sameOrigin reports whether a handshake came from this dashboard. Browsers
// always send Origin on a WebSocket handshake and scripts cannot forge it,
// which is what makes this the defence against cross-site WebSocket
// hijacking; a client that sends none is not a browser and cannot be a
// cross-site victim.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// wsConn serialises writes: gorilla allows only one writer at a time, and
// here the pod's output, the control frames and the idle timer all write.
type wsConn struct {
	mu sync.Mutex
	c  *websocket.Conn
}

// Write sends terminal bytes. It is the exec stream's stdout, so the bytes go
// out as a binary frame: a text frame would force UTF-8 and corrupt whatever
// a program prints.
func (w *wsConn) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsConn) control(v shellControl) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.WriteMessage(websocket.TextMessage, b)
}

// ping is a keepalive, not a liveness probe: nothing waits for the pong. Its
// only job is to put a byte on the wire often enough that a proxy counting
// idle seconds does not reap a session someone is still watching.
func (w *wsConn) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
}

func (w *wsConn) close(code int, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(reason) > 120 {
		reason = reason[:120] // a close reason is capped at 125 bytes
	}
	return w.c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}
