package api

import (
	"context"
	"encoding/json"
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

func TestShellSurvivesResizeAfterExit(t *testing.T) {
	// Review Focus 5: the process is already gone when a resize arrives. The
	// reader goroutine is the only closer of the size channel for exactly this
	// reason; a send on a closed channel would panic in a bare goroutine,
	// which gin's Recovery does not catch, and crash the test binary here.
	f := newShellFixture(t, "", nil)
	f.s.execStream = func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error {
		return nil // exits immediately, before any resize can arrive
	}
	c := f.dial(t, "web.1")
	defer c.Close()
	readControl(t, c) // open
	for {
		if m := readControl(t, c); m["type"] == "exit" {
			break
		}
	}
	for i := 0; i < 20; i++ {
		if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
			return // the server closed the socket, which is fine
		}
	}
	// Give the reader goroutine time to handle them and panic if it is going to.
	time.Sleep(100 * time.Millisecond)
	if !f.s.shells.held(actorKey(ext.Identity{Subject: "admin-token", Provider: "token"}), "blog") {
		return // session finished cleanly
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
}
