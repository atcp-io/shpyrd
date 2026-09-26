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
	project_ "shpyrd/pkg/project"
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

// shellWriteWait bounds every write to the socket. Without it a client that
// has stopped reading — a suspended laptop, a throttled background tab, a slow
// link under a chatty process, or someone doing it on purpose — fills its
// receive window and parks the exec stream's stdout copy inside Write while it
// holds wsConn.mu. Everything else then queues behind that lock, the idle
// reaper included, so the session can never be reaped: four goroutines, an
// io.Pipe and an apiserver exec connection leak, and the user is locked out of
// their own project's shell until the control plane restarts, told to close a
// shell they have no way to close. Two seconds is far longer than a terminal
// frame needs and short enough that a wedged peer is let go promptly.
const shellWriteWait = 2 * time.Second

// shellProbeTimeout bounds resolving the shell. The probe runs up to four
// pods/exec calls before the idle timer is armed, so a pod on a NotReady node
// or an apiserver that accepts and then stalls would otherwise leave the
// browser at "Connecting to web.1…" forever with the one shell slot claimed
// and nothing able to reclaim it.
const shellProbeTimeout = 15 * time.Second

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

// execRunFunc is the one-shot exec resolveShell probes with. It is a seam of
// its own, below probeShellFunc: tests that replace probeShell skip the
// candidate walk entirely, and that walk — which candidate wins, and which
// errors abort rather than continue — is where this branch deliberately departs
// from `shpyrd shell`.
type execRunFunc func(ctx context.Context, namespace, pod, container string, command []string) (string, error)

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
	slug := c.Param("slug")
	// The same slug hygiene s.require applies everywhere else. This route has no
	// middleware, so it repeats it: defence in depth, checked before the ticket
	// is redeemed so a malformed path cannot even burn one. A bad slug cannot
	// get past the ticket's project binding either, but the one route without a
	// gate in front of it should not be the one route that trusts its input.
	if !project_.ValidSlug(slug) || strings.HasPrefix(slug, "app-") {
		abort(c, http.StatusNotFound, errors.New("project not found (paths take the project slug, not its namespace)"))
		return
	}
	t, err := s.execTickets.redeem(c.Query("ticket"))
	if err != nil {
		abort(c, http.StatusForbidden, err)
		return
	}
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

	command, err := s.probe(ctx, namespace, pod)
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
	//
	// closed says the client's side went away (tab closed, network gone), which
	// cancels the session. Closing stdin is not enough on its own: a foreground
	// process that ignores stdin and prints nothing — `sleep`, `tail -f`, a
	// wedged process — would keep the exec stream, the pod's command and the
	// user's one shell slot alive until the 30-minute reap, though the spec
	// makes "sessions end cleanly on tab close" a goal.
	closed := make(chan struct{})
	go func() {
		// Ordered so the exit path below can read closed the moment ctx is
		// cancelled: close(closed) runs before cancel().
		defer cancel()
		defer close(closed)
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
	//
	// The reaper writes nothing itself. Telling the browser why is a write, and a
	// write waits for wsConn.mu, which a peer that has stopped reading can hold
	// for a whole shellWriteWait: a reap that begins with a write is a reap that
	// can be delayed by the very client it is trying to get rid of. So it only
	// reaps, and the exit path below — which unblocks as soon as the cancelled
	// stream returns — sends the explanation, in a guaranteed order ahead of the
	// close frame rather than racing the handler's own teardown.
	idled := make(chan struct{})
	go func() {
		select {
		case <-idle.C:
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
		// Most specific reason first. An idle reap outranks a client close
		// because the reap is what ended the session: closing the socket makes
		// the browser's side go away a moment later, so both can be signalled
		// and only the first one is the cause. The fall-through is the request
		// context itself going away, which today means a server shutdown.
		detail = "closed: server shutting down"
		switch {
		case signalled(idled):
			detail = fmt.Sprintf("closed: idle for %s", s.shellIdle)
			// Why, before the close: a terminal that vanishes without a word
			// reads as a bug to the person it happens to.
			_ = w.control(shellControl{Type: "error", Message: fmt.Sprintf("shell closed after %s idle", s.shellIdle)})
		case signalled(closed):
			detail = "closed: the client disconnected"
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

// signalled reports whether ch has been closed, without waiting for it.
func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// probe resolves the shell under a deadline. The work runs in its own
// goroutine and the answer comes back over a buffered channel for two reasons:
// the probe happens before the idle timer is armed, so nothing else can reclaim
// the session while it hangs, and the handler must be able to give up on it
// even if the exec call underneath were to ignore its context. The goroutine
// then finishes into the buffer with nobody listening instead of leaking on a
// send; with the real kexec.Run it returns as soon as the deadline cancels ctx.
func (s *Server) probe(ctx context.Context, namespace, pod string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.shellProbe)
	defer cancel()
	type result struct {
		command []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		command, err := s.probeShell(ctx, namespace, pod, appContainer)
		done <- result{command, err}
	}()
	select {
	case r := <-done:
		return r.command, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Named as a timeout: "no usable shell in the image" would blame the
			// image for an instance that simply never answered.
			return nil, fmt.Errorf("timed out after %s looking for a shell on the instance; it may be unreachable", s.shellProbe)
		}
		return nil, ctx.Err()
	}
}

// streamExec is the real bridge: an exec stream with a TTY.
func (s *Server) streamExec(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
	u := kexec.ExecURL(s.kube, namespace, pod, container, command, true)
	// With a TTY there is one stream: the pty folds stderr into stdout, and
	// client-go ignores StreamOptions.Stderr entirely when Tty is set. So nil,
	// rather than stdout a second time: passing a writer that can never be
	// written to only suggests the terminal has two output paths.
	return kexec.StreamIO(ctx, s.kube, u, true, stdin, stdout, nil, sizes)
}

// resolveShell reports the first candidate the image has. Each probe is a
// throwaway non-TTY exec that exits at once, so a missing shell never writes
// anything into the terminal the user is about to see.
//
// Only a "not found" failure moves on to the next candidate. Anything else —
// pods/exec forbidden, the apiserver unreachable, the pod gone — aborts: the
// image's shells are not in question then, and walking the remaining candidates
// would turn one honest error into "no usable shell in the image", which sends
// the user looking at the wrong thing.
func (s *Server) resolveShell(ctx context.Context, namespace, pod, container string) ([]string, error) {
	var lastErr error
	for _, cand := range shellCandidates {
		probe := append(append([]string{}, cand...), "-c", "exit 0")
		if _, err := s.execRun(ctx, namespace, pod, container, probe); err == nil {
			return cand, nil
		} else if !kexec.IsNotFound(err) {
			return nil, err
		} else {
			lastErr = err
		}
	}
	return nil, fmt.Errorf("%w: %v", errNoShell, lastErr)
}

// runKexec is the real probe behind execRun: one throwaway non-TTY exec.
func (s *Server) runKexec(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
	return kexec.Run(ctx, s.kube, namespace, pod, container, command)
}

// sameOrigin reports whether the handshake's Origin names this server's own
// host — host only: not the scheme, not the port, and not "this dashboard",
// since the engine answers on every host that reaches it. That is enough here
// because the ticket is the entire credential and no cookie is involved, so
// there is no ambient authority for another page on a sibling host to borrow;
// the check exists to stop the conventional cross-site WebSocket hijack, where
// a page elsewhere dials this socket with the victim's ambient credentials.
// Browsers always send Origin on a WebSocket handshake and scripts cannot forge
// it; a client that sends none is not a browser and cannot be a cross-site
// victim.
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
//
// Every write takes a deadline (shellWriteWait). The mutex is what makes that
// mandatory rather than merely tidy: a peer that stops reading would otherwise
// park one writer inside Write forever with the lock held, and a lock nobody
// can take is how a session becomes unreapable and its slot unrecoverable.
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
	_ = w.c.SetWriteDeadline(time.Now().Add(shellWriteWait))
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
	_ = w.c.SetWriteDeadline(time.Now().Add(shellWriteWait))
	return w.c.WriteMessage(websocket.TextMessage, b)
}

// ping is a keepalive, not a liveness probe: nothing waits for the pong. Its
// only job is to put a byte on the wire often enough that a proxy counting
// idle seconds does not reap a session someone is still watching.
func (w *wsConn) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(shellWriteWait)
	_ = w.c.SetWriteDeadline(deadline)
	return w.c.WriteControl(websocket.PingMessage, nil, deadline)
}

func (w *wsConn) close(code int, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(reason) > 120 {
		reason = reason[:120] // a close reason is capped at 125 bytes
	}
	deadline := time.Now().Add(shellWriteWait)
	_ = w.c.SetWriteDeadline(deadline)
	return w.c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), deadline)
}
