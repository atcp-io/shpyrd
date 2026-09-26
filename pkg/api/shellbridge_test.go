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

func newShellFixture(t *testing.T, out string, execErr error) *shellFixture {
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
		return execErr
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
	f := newShellFixture(t, "hello", nil)
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
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestShellReaderExitsWhenProcessStopsReading(t *testing.T) {
	// A process can stop reading stdin long before it exits (`less` after
	// 'q'). Whatever is typed then sits in the stdin pipe with nobody to take
	// it, and the reader goroutine parks in Write, where closing the socket
	// cannot reach it. Closing the pipe's read half once the stream is done is
	// what lets that goroutine go; without it every such session leaks a
	// goroutine and its pipe for the life of the server.
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, raw, nil)
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

func TestShellForwardsResizeAndIgnoresLateOnes(t *testing.T) {
	// Review Focus 5: a resize arriving after the process exits is dropped,
	// not written to a closed channel.
	f := newShellFixture(t, "", nil)
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.sizes)
		f.mu.Unlock()
		if n == 1 {
			f.mu.Lock()
			got := f.sizes[0]
			f.mu.Unlock()
			if got.Width != 120 || got.Height != 40 {
				t.Errorf("size = %+v, want 120x40", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the resize was never forwarded")
}

func TestShellReportsExitCode(t *testing.T) {
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
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
	f := newShellFixture(t, "", nil)
	f.s.shellIdle = 50 * time.Millisecond
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c)
	m := readControl(t, c)
	if m["type"] != "error" || !strings.Contains(m["message"].(string), "idle") {
		t.Errorf("expected an idle error frame, got %v", m)
	}
	// The browser is told it was reaped for being idle; the trail has to say
	// the same, or an operator cannot tell this from a server shutdown. The
	// fake exec above ignores ctx where the real StreamIO returns on cancel, so
	// the socket has to close here to let the session finish and audit.
	c.Close()
	waitFor(t, "the idle reap to be audited", func() bool {
		return len(auditDetails(t, f, "shell.close")) > 0
	})
	if got := auditDetails(t, f, "shell.close"); !strings.Contains(got[0], "idle") {
		t.Errorf("audit detail = %q, want it to name the idle reap rather than a bare close", got)
	}
}
