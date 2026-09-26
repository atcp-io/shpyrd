package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/audit"
	"shpyrd/pkg/ext"
	"shpyrd/pkg/kexec"
)

// shellFixture is a server whose exec is faked, plus an httptest server to
// dial a real WebSocket against.
type shellFixture struct {
	s    *Server
	http *httptest.Server
	// recorded by the fake exec
	mu      sync.Mutex
	stdin   []byte
	sizes   []remotecommand.TerminalSize
	command []string
}

func newShellFixture(t *testing.T, out string) *shellFixture {
	t.Helper()
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))
	f := &shellFixture{s: s}
	s.probeShell = func(ctx context.Context, namespace, pod, container string) ([]string, error) {
		return []string{"bash"}, nil
	}
	s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		f.mu.Lock()
		f.command = command
		f.mu.Unlock()
		if out != "" {
			_, _ = stdout.Write([]byte(out))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for sz := range sizes {
				f.mu.Lock()
				f.sizes = append(f.sizes, sz)
				f.mu.Unlock()
			}
		}()
		b, _ := io.ReadAll(stdin)
		f.mu.Lock()
		f.stdin = append(f.stdin, b...)
		f.mu.Unlock()
		<-done
		return nil
	}
	f.http = httptest.NewServer(s.Handler())
	t.Cleanup(f.http.Close)
	return f
}

// dial opens a shell WebSocket with a freshly minted ticket.
func (f *shellFixture) dial(t *testing.T, instance string) *websocket.Conn {
	t.Helper()
	code, err := f.s.execTickets.mint(execTicket{
		Identity: ext.Identity{Subject: "admin-token", Provider: "token", Admin: true},
		Project:  "blog", Instance: instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f.dialWith(t, instance, code)
}

func (f *shellFixture) dialWith(t *testing.T, instance, code string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(f.http.URL, "http") +
		"/api/projects/blog/shell?instance=" + instance + "&ticket=" + code
	c, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	return c
}

// readControl reads frames until a text frame arrives and returns it decoded.
func readControl(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		typ, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.TextMessage {
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("control frame %q: %v", data, err)
			}
			return m
		}
	}
}

func TestShellOpensAndExits(t *testing.T) {
	f := newShellFixture(t, "hello")
	c := f.dial(t, "web.1")
	defer c.Close()

	open := readControl(t, c)
	if open["type"] != "open" || open["instance"] != "web.1" || open["shell"] != "bash" {
		t.Fatalf("open frame = %v", open)
	}
	// The client closing is how a tab close ends a session.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	c.Close()

	// The registry must not keep the slot once the session is over.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !f.s.shells.held(actorKey(ext.Identity{Subject: "admin-token", Provider: "token"}), "blog") {
			// The slot is released only after the exec stream returned, so
			// what the client typed has by now reached its stdin.
			f.mu.Lock()
			defer f.mu.Unlock()
			if string(f.stdin) != "exit\n" {
				t.Errorf("stdin = %q, want %q", f.stdin, "exit\n")
			}
			if len(f.command) == 0 || f.command[len(f.command)-1] != "bash" {
				t.Errorf("command = %v, want it to end in the resolved shell", f.command)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the shell slot was never released")
}

// pipeWriters counts the goroutines parked writing to an io.Pipe, which is
// where a leaked terminal reader shows up.
func pipeWriters() int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "io.(*pipe).write")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitUntil(t, 3*time.Second, what, cond)
}

// waitUntil is waitFor with a budget, for the cases that have to outlast a
// write deadline.
func waitUntil(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// adminActor is the identity the fixture's tickets carry.
func adminActor() string {
	return actorKey(ext.Identity{Subject: "admin-token", Provider: "token"})
}

func TestShellReaderExitsWhenProcessStopsReading(t *testing.T) {
	// A process can stop reading stdin long before it exits (`less` after
	// 'q'). Whatever is typed then sits in the stdin pipe with nobody to take
	// it, and the reader goroutine parks in Write, where closing the socket
	// cannot reach it. Closing the pipe's read half once the stream is done is
	// what lets that goroutine go; without it every such session leaks a
	// goroutine and its pipe for the life of the server.
	f := newShellFixture(t, "")
	release := make(chan struct{})
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		<-release // never reads stdin
		return nil
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c) // open

	before := pipeWriters()
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("typed at a process that stopped reading\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the reader to park on the stdin pipe", func() bool { return pipeWriters() > before })

	close(release) // the process exits with that keystroke still unread
	actor := actorKey(ext.Identity{Subject: "admin-token", Provider: "token"})
	waitFor(t, "the session to end", func() bool { return !f.s.shells.held(actor, "blog") })
	waitFor(t, "the reader goroutine to exit", func() bool { return pipeWriters() <= before })
}

func TestShellPassesBinaryOutputThrough(t *testing.T) {
	// Review Focus 3: a byte sequence that is not valid UTF-8 must arrive
	// unchanged, so `cat` on a binary does not come out mangled.
	raw := string([]byte{0x00, 0xff, 0xfe, 'a', 0x1b, '[', '0', 'm'})
	f := newShellFixture(t, raw)
	c := f.dial(t, "web.1")
	defer c.Close()
	if open := readControl(t, c); open["type"] != "open" {
		t.Fatalf("open frame = %v", open)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		typ, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.BinaryMessage {
			if string(data) != raw {
				t.Errorf("output = %q, want %q", data, raw)
			}
			return
		}
	}
}

func TestShellForwardsResizeAndIgnoresMalformedFrames(t *testing.T) {
	// One well-formed resize reaches the size queue, and the malformed control
	// frames after it are ignored rather than ending the session: a buggy client
	// must not be able to kill someone's terminal.
	f := newShellFixture(t, "")
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c)
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	// Garbage control frames must not kill the session either.
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":"wide"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(`not json`)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	c.Close()

	waitFor(t, "the resize to reach the size queue", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.sizes) > 0
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sizes) != 1 {
		t.Fatalf("sizes = %+v, want exactly the one well-formed resize", f.sizes)
	}
	if f.sizes[0].Width != 120 || f.sizes[0].Height != 40 {
		t.Errorf("size = %+v, want 120x40", f.sizes[0])
	}
}

func TestShellReportsExitCode(t *testing.T) {
	f := newShellFixture(t, "")
	// This fake returns at once rather than waiting for stdin to close, so the
	// test sees the exit frame without having to close the socket first.
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		return &kexec.ExitError{Code: 3}
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c)
	for {
		m := readControl(t, c)
		if m["type"] == "exit" {
			if m["code"] != float64(3) {
				t.Errorf("exit frame = %v, want code 3", m)
			}
			return
		}
	}
}

func TestShellReportsStreamFailureAsError(t *testing.T) {
	// A broken stream carries no remote exit status, so it must not be dressed
	// up as one: an apiserver refusal or a mid-session SPDY break is an error
	// frame, and the audit trail gets the reason rather than a fabricated
	// "exit 1" that an operator cannot tell from a user typing `exit 1`.
	f := newShellFixture(t, "")
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		return errors.New(`pods/exec is forbidden: User "x" cannot create resource`)
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c) // open
	m := readControl(t, c)
	if m["type"] != "error" {
		t.Fatalf("frame = %v, want an error frame and not an exit", m)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "forbidden") {
		t.Errorf("message = %q, want the stream failure in it", msg)
	}
}

func TestShellPingsToSurviveAProxy(t *testing.T) {
	// A live but quiet terminal has to outlast ingress-nginx's 60s
	// proxy_read_timeout, and only the server can ping.
	f := newShellFixture(t, "")
	f.s.shellPing = 20 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		<-release // hold the session open, writing nothing
		return nil
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c) // open

	// Control frames are only processed while a read is in flight.
	pinged := make(chan struct{}, 1)
	c.SetPingHandler(func(string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return nil
	})
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case <-pinged:
	case <-time.After(3 * time.Second):
		t.Error("no ping arrived: a quiet session would be reaped by the proxy")
	}
}

// auditDetails returns the detail recorded for every audit event of the given
// action on project blog, which is where shell.open and shell.close land.
func auditDetails(t *testing.T, f *shellFixture, action string) []string {
	t.Helper()
	evs, err := f.s.kube.Kube.CoreV1().Events("app-blog").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs.Items {
		if e.Annotations[audit.AnnotationAction] == action {
			out = append(out, e.Annotations[audit.AnnotationDetail])
		}
	}
	return out
}

func TestShellAuditsAProbeFailure(t *testing.T) {
	// No terminal opens, but resolving the shell has already run real execs
	// against a running pod and minting the ticket records nothing, so this is
	// the only chance to leave a trace of who tried what — and an RBAC or
	// apiserver failure behind those probes is exactly what an operator needs
	// to see.
	f := newShellFixture(t, "")
	f.s.probeShell = func(ctx context.Context, namespace, pod, container string) ([]string, error) {
		return nil, errNoShell
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	if m := readControl(t, c); m["type"] != "error" {
		t.Fatalf("frame = %v, want an error frame", m)
	}
	waitFor(t, "the failed attempt to be audited", func() bool {
		return len(auditDetails(t, f, "shell.close")) > 0
	})
	if got := auditDetails(t, f, "shell.close"); !strings.Contains(got[0], "no usable shell") {
		t.Errorf("audit detail = %q, want the probe failure named", got)
	}
	if got := auditDetails(t, f, "shell.open"); len(got) != 0 {
		t.Errorf("shell.open was audited though no terminal ever opened: %q", got)
	}
}

func TestShellCandidateOrder(t *testing.T) {
	// The order is the contract with `shpyrd shell` (internal/cli/shell.go),
	// not the constant, and the two are deliberately not shared: the launcher
	// comes first so a buildpack image gets its environment, and bash before sh
	// so a user gets the better shell where both exist. Reordering this table
	// silently diverges the web terminal from the CLI, which is why it is
	// pinned here rather than left to the reader.
	want := [][]string{
		{cnbLauncher, "--", "bash"},
		{"bash"},
		{cnbLauncher, "--", "sh"},
		{"sh"},
	}
	if len(shellCandidates) != len(want) {
		t.Fatalf("shellCandidates = %v, want %v", shellCandidates, want)
	}
	for i := range want {
		if strings.Join(shellCandidates[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Errorf("candidate %d = %v, want %v", i, shellCandidates[i], want[i])
		}
	}
}

func TestShellReportsNoUsableShell(t *testing.T) {
	// Review Focus 2: a distroless image gets a readable error, not silence.
	f := newShellFixture(t, "")
	f.s.probeShell = func(ctx context.Context, namespace, pod, container string) ([]string, error) {
		return nil, errNoShell
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	m := readControl(t, c)
	if m["type"] != "error" || !strings.Contains(m["message"].(string), "no usable shell in the image") {
		t.Errorf("error frame = %v", m)
	}
}

func TestShellRefusesMissingInstance(t *testing.T) {
	// Review Focus 1: the instance went away during a rolling deploy.
	f := newShellFixture(t, "")
	code, err := f.s.execTickets.mint(execTicket{
		Identity: ext.Identity{Subject: "admin-token", Provider: "token", Admin: true},
		Project:  "blog", Instance: "web.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	u := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/blog/shell?instance=web.9&ticket=" + code
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("the dial should have been refused")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v, want 404", resp)
	}
}

func TestShellRefusesTicketMismatchAndReplay(t *testing.T) {
	f := newShellFixture(t, "")
	code, _ := f.s.execTickets.mint(execTicket{
		Identity: ext.Identity{Subject: "admin-token", Provider: "token", Admin: true},
		Project:  "blog", Instance: "web.1",
	})
	// A ticket for web.1 must not open web.2.
	u := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/blog/shell?instance=web.2&ticket=" + code
	if _, resp, err := websocket.DefaultDialer.Dial(u, nil); err == nil {
		t.Error("a ticket bound to another instance was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
	// And it is spent, even though it was not honoured.
	u = "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/blog/shell?instance=web.1&ticket=" + code
	if _, resp, err := websocket.DefaultDialer.Dial(u, nil); err == nil {
		t.Error("a spent ticket was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("replay status = %v, want 403", resp)
	}
}

func TestShellRefusesRevokedRole(t *testing.T) {
	// Review Focus 4: the grant went away inside the ticket's 30 seconds.
	f := newShellFixture(t, "")
	f.s.authz.TTL = 1
	if rec := do(t, f.s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	code, _ := f.s.execTickets.mint(execTicket{
		Identity: ext.Identity{Subject: "u9", Email: "gone@example.test", Provider: "local"},
		Project:  "blog", Instance: "web.1",
	})
	u := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/blog/shell?instance=web.1&ticket=" + code
	if _, resp, err := websocket.DefaultDialer.Dial(u, nil); err == nil {
		t.Error("a caller with no grant was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
}

func TestShellRefusesForeignOrigin(t *testing.T) {
	f := newShellFixture(t, "")
	code, _ := f.s.execTickets.mint(execTicket{
		Identity: ext.Identity{Subject: "admin-token", Provider: "token", Admin: true},
		Project:  "blog", Instance: "web.1",
	})
	u := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/blog/shell?instance=web.1&ticket=" + code
	h := http.Header{"Origin": []string{"https://evil.test"}}
	if _, resp, err := websocket.DefaultDialer.Dial(u, h); err == nil {
		t.Error("a cross-origin handshake was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
}

func TestShellIdleTimeout(t *testing.T) {
	f := newShellFixture(t, "")
	f.s.shellIdle = 50 * time.Millisecond
	// Like the real StreamIO, this stream returns when the session is cancelled.
	// That matters here: the reap does no writes of its own — a write can be
	// held up by the very client being reaped — so the explanation goes out from
	// the exit path once the cancelled stream has returned.
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		<-ctx.Done()
		return ctx.Err()
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c)
	m := readControl(t, c)
	if m["type"] != "error" || !strings.Contains(m["message"].(string), "idle") {
		t.Errorf("expected an idle error frame, got %v", m)
	}
	// The browser is told it was reaped for being idle; the trail has to say
	// the same, or an operator cannot tell this from a server shutdown.
	waitFor(t, "the idle reap to be audited", func() bool {
		return len(auditDetails(t, f, "shell.close")) > 0
	})
	if got := auditDetails(t, f, "shell.close"); !strings.Contains(got[0], "idle") {
		t.Errorf("audit detail = %q, want it to name the idle reap rather than a bare close", got)
	}
}

func TestShellReleasesTheSlotWhenTheClientStopsReading(t *testing.T) {
	// A peer that stops reading — a suspended laptop, a throttled background
	// tab, or someone doing it on purpose — fills its receive window, and
	// without a write deadline the stdout copy parks inside Write holding
	// wsConn.mu. Every other write then queues behind that lock, the idle
	// reaper's included, so the session can never be reaped: the goroutines, the
	// pipe and the apiserver exec leak, and the user is locked out of their own
	// project's shell until the control plane restarts, told to close a shell
	// they have no way to close. This client never reads a single frame while
	// the stream writes far more than any socket buffer holds.
	f := newShellFixture(t, "")
	f.s.shellIdle = 100 * time.Millisecond
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		chunk := make([]byte, 64<<10)
		for i := 0; i < 512; i++ { // 32 MiB, against a client reading nothing
			if _, err := stdout.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	// Deliberately no reads at all here, not even the open frame.

	// Generous, but bounded: the deadline is two seconds and the reap follows.
	waitUntil(t, 10*time.Second, "the shell slot to be released", func() bool {
		return !f.s.shells.held(adminActor(), "blog")
	})
}

func TestShellEndsWhenTheClientDisconnects(t *testing.T) {
	// Closing stdin is not enough on its own to end a session: a foreground
	// process that ignores stdin and prints nothing (`sleep`, `tail -f`, a
	// wedged process) keeps the exec stream open, so the pod goes on running it
	// and the user's one slot stays held until the 30-minute reap, though the
	// spec makes "sessions end cleanly on tab close" a goal.
	//
	// The session does in fact end today even without the reader's cancel, but
	// only by accident: gorilla reads through the buffered reader net/http hands
	// out on Hijack, and the EOF that reader sees cancels the request context.
	// That is undocumented, it does not hold if the reader is parked writing to
	// stdin rather than reading, and it says nothing in the audit trail. So this
	// test pins both halves — the slot is released, and the trail names the
	// client disconnecting rather than borrowing the shutdown wording.
	f := newShellFixture(t, "")
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		<-ctx.Done() // never returns of its own accord
		return ctx.Err()
	}
	c := f.dial(t, "web.1")
	readControl(t, c) // open
	c.Close()

	waitFor(t, "the session to end", func() bool { return !f.s.shells.held(adminActor(), "blog") })
	waitFor(t, "the close to be audited", func() bool { return len(auditDetails(t, f, "shell.close")) > 0 })
	// The trail should say the client went away, not borrow the shutdown wording.
	if got := auditDetails(t, f, "shell.close"); !strings.Contains(got[0], "client disconnected") {
		t.Errorf("audit detail = %q, want it to name the client disconnecting", got)
	}
}

func TestShellProbeTimesOut(t *testing.T) {
	// Resolving the shell is up to four pods/exec calls made before the idle
	// timer is armed. A pod on a NotReady node, or an apiserver that accepts and
	// then stalls, would otherwise leave the browser at "Connecting to web.1..."
	// indefinitely with the slot claimed and nothing able to reclaim it.
	f := newShellFixture(t, "")
	f.s.shellProbe = 50 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.s.probeShell = func(ctx context.Context, namespace, pod, container string) ([]string, error) {
		<-release
		return []string{"bash"}, nil
	}
	c := f.dial(t, "web.1")
	defer c.Close()

	m := readControl(t, c)
	if m["type"] != "error" {
		t.Fatalf("frame = %v, want an error frame", m)
	}
	// Named as a timeout: "no usable shell" would blame the image for an
	// instance that simply never answered.
	if msg, _ := m["message"].(string); !strings.Contains(msg, "timed out") {
		t.Errorf("message = %q, want the timeout named", msg)
	}
	waitFor(t, "the shell slot to be released", func() bool { return !f.s.shells.held(adminActor(), "blog") })
}

func TestShellServerOutputDoesNotResetTheIdleTimer(t *testing.T) {
	// A design bullet of the RFC: the timer follows client input only, so a
	// chatty process cannot hold a terminal open for someone who has walked
	// away. Nothing else pins this down, and a refactor that reset the timer on
	// every write would pass every other test in this file.
	f := newShellFixture(t, "")
	f.s.shellIdle = 150 * time.Millisecond
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := stdout.Write([]byte("still printing\r\n")); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c) // open

	// readControl consumes the output frames while waiting for a text frame, so
	// the client is reading throughout: the only thing not happening is typing.
	m := readControl(t, c)
	if m["type"] != "error" || !strings.Contains(m["message"].(string), "idle") {
		t.Errorf("frame = %v, want the idle reap despite the output", m)
	}
}

func TestShellRefusesAnOversizedFrame(t *testing.T) {
	// The read limit bounds what one client can make this single-replica server
	// allocate; an oversized frame is a read error that ends that session.
	f := newShellFixture(t, "")
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		<-ctx.Done()
		return ctx.Err()
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c) // open
	if err := c.WriteMessage(websocket.BinaryMessage, make([]byte, shellReadLimit+1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the oversized frame to end the session", func() bool {
		return !f.s.shells.held(adminActor(), "blog")
	})
}

func TestShellRefusesASecondSocket(t *testing.T) {
	// Only the mint-time refusal was covered. Two tickets minted before either
	// socket exists take that check out of the way, leaving the socket's own
	// claim as the thing that has to refuse.
	f := newShellFixture(t, "")
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		<-hold
		return nil
	}
	id := ext.Identity{Subject: "admin-token", Provider: "token", Admin: true}
	first, err := f.s.execTickets.mint(execTicket{Identity: id, Project: "blog", Instance: "web.1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.s.execTickets.mint(execTicket{Identity: id, Project: "blog", Instance: "web.1"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.dialWith(t, "web.1", first)
	defer c.Close()
	readControl(t, c) // open: the slot is held from here

	u := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/blog/shell?instance=web.1&ticket=" + second
	if _, resp, err := websocket.DefaultDialer.Dial(u, nil); err == nil {
		t.Error("a second concurrent socket was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %v, want 409", resp)
	}
}

func TestShellOpensForADeveloper(t *testing.T) {
	// The flow this whole design exists for: not the admin token but a person
	// with a dashboard account and the developer role on one project, minting
	// the ticket over their session and then dialling with it.
	f := newShellFixture(t, "")
	f.s.authz.TTL = 1
	if rec := do(t, f.s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, f.s, "POST", "/api/projects/blog/members", `{"role":"developer","user":"dev@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add developer: %d %s", rec.Code, rec.Body.String())
	}
	dev := ext.Identity{Subject: "u7", Email: "dev@example.test", Provider: "local"}
	sid, csrf := signIn(t, f.s, dev)
	rec := doCookie(t, f.s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", sid, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("developer mint: %d %s", rec.Code, rec.Body.String())
	}
	var body struct{ Ticket string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	c := f.dialWith(t, "web.1", body.Ticket)
	defer c.Close()
	if open := readControl(t, c); open["type"] != "open" || open["instance"] != "web.1" {
		t.Fatalf("open frame = %v", open)
	}
	// The slot belongs to the developer, not to whoever minted last.
	if !f.s.shells.held(actorKey(dev), "blog") {
		t.Error("the shell slot was not claimed for the developer")
	}
}

// notFound is what a missing executable looks like coming back from an exec.
var notFound = errors.New(`exec: "bash": executable file not found in $PATH`)

// resolveFixture is a server whose one-shot exec is faked, recording every
// candidate resolveShell probes. It exercises the candidate walk itself, which
// every test replacing probeShell skips entirely.
func resolveFixture(t *testing.T, answer func(command []string) error) (*Server, *[][]string) {
	t.Helper()
	s, _ := newTestServer(t, nil, nil)
	var tried [][]string
	s.execRun = func(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
		tried = append(tried, command)
		return "", answer(command)
	}
	return s, &tried
}

func TestShellResolvePicksTheLauncherFirst(t *testing.T) {
	// A buildpack image: the launcher answers, so it wins outright and nothing
	// else is probed — that is what gets the process its build environment.
	s, tried := resolveFixture(t, func([]string) error { return nil })
	got, err := s.resolveShell(context.Background(), "app-blog", "blog-web-aaa", appContainer)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != cnbLauncher+" -- bash" {
		t.Errorf("shell = %v, want the launcher with bash", got)
	}
	if len(*tried) != 1 {
		t.Fatalf("probed %v, want only the first candidate", *tried)
	}
	// Each probe is a throwaway command that exits at once, so a missing shell
	// never writes into the terminal the user is about to see.
	want := append(append([]string{}, shellCandidates[0]...), "-c", "exit 0")
	if strings.Join((*tried)[0], " ") != strings.Join(want, " ") {
		t.Errorf("probe = %v, want %v", (*tried)[0], want)
	}
}

func TestShellResolveFallsBackToBash(t *testing.T) {
	// A plain image has no /cnb/lifecycle/launcher.
	s, tried := resolveFixture(t, func(command []string) error {
		if command[0] == cnbLauncher {
			return notFound
		}
		return nil
	})
	got, err := s.resolveShell(context.Background(), "app-blog", "blog-web-aaa", appContainer)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "bash" {
		t.Errorf("shell = %v, want bash", got)
	}
	if len(*tried) != 2 {
		t.Errorf("probed %v, want the launcher then bash", *tried)
	}
}

func TestShellResolveFallsBackToSh(t *testing.T) {
	// busybox and friends: no launcher, no bash, but sh is there.
	s, tried := resolveFixture(t, func(command []string) error {
		if command[0] == cnbLauncher || command[0] == "bash" {
			return notFound
		}
		return nil
	})
	got, err := s.resolveShell(context.Background(), "app-blog", "blog-web-aaa", appContainer)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "sh" {
		t.Errorf("shell = %v, want sh", got)
	}
	if len(*tried) != 4 {
		t.Errorf("probed %v, want all four candidates in order", *tried)
	}
}

func TestShellResolveReportsNoUsableShell(t *testing.T) {
	// A distroless image has none of the four.
	s, tried := resolveFixture(t, func([]string) error { return notFound })
	_, err := s.resolveShell(context.Background(), "app-blog", "blog-web-aaa", appContainer)
	if !errors.Is(err, errNoShell) {
		t.Fatalf("err = %v, want errNoShell", err)
	}
	if len(*tried) != 4 {
		t.Errorf("probed %v, want all four tried before giving up", *tried)
	}
}

func TestShellResolveAbortsOnARealFailure(t *testing.T) {
	// Anything but a missing executable stops the walk: pods/exec forbidden or
	// an apiserver that cannot be reached says nothing about the image's shells,
	// and reporting it as "no usable shell in the image" would send the user
	// looking at the wrong thing — while three more execs went out for nothing.
	forbidden := errors.New(`pods/exec is forbidden: User "x" cannot create resource`)
	s, tried := resolveFixture(t, func([]string) error { return forbidden })
	_, err := s.resolveShell(context.Background(), "app-blog", "blog-web-aaa", appContainer)
	if !errors.Is(err, forbidden) {
		t.Fatalf("err = %v, want the refusal itself", err)
	}
	if errors.Is(err, errNoShell) {
		t.Error("a refusal was dressed up as a missing shell")
	}
	if len(*tried) != 1 {
		t.Errorf("probed %v, want the walk to stop at the first real failure", *tried)
	}
}

func TestShellRefusesANamespaceAsASlug(t *testing.T) {
	// The socket is the one route with no middleware in front of it, so it
	// repeats the slug hygiene s.require applies everywhere else — and does it
	// before redeeming, so a malformed path cannot even burn a ticket.
	f := newShellFixture(t, "")
	code, err := f.s.execTickets.mint(execTicket{
		Identity: ext.Identity{Subject: "admin-token", Provider: "token", Admin: true},
		Project:  "blog", Instance: "web.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	u := "ws" + strings.TrimPrefix(f.http.URL, "http") + "/api/projects/app-blog/shell?instance=web.1&ticket=" + code
	if _, resp, err := websocket.DefaultDialer.Dial(u, nil); err == nil {
		t.Error("a namespace passed as a slug was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %v, want 404", resp)
	}
	// Still spendable: the refusal came before redemption.
	if _, err := f.s.execTickets.redeem(code); err != nil {
		t.Errorf("the ticket was burned by a malformed path: %v", err)
	}
}
