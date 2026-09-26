# Web Terminal (RFC-0026) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give dashboard users a real shell into a running instance of a project, from a Shell tab, gated by the `project.exec` role and audited.

**Architecture:** Three routes on the existing gin server. `GET /instances` feeds an instance selector; `POST /shell/ticket` mints a one-time code; `GET /shell?ticket=…` upgrades to a WebSocket and bridges it to `pods/exec` with a TTY. The bridge streams over a new `kexec.StreamIO` primitive extracted from the CLI's `kexec.Stream`, so both callers share one WebSocket-with-SPDY-fallback executor. The browser side is xterm.js in a lazily loaded component.

**Tech Stack:** Go 1.x, gin, client-go `remotecommand`, `github.com/gorilla/websocket` (currently an indirect dependency, promoted to direct), React 19, xterm.js (`@xterm/xterm`, `@xterm/addon-fit`), TanStack Query, Tailwind.

**Spec:** `rfcs/0026-web-terminal.md`

## Global Constraints

- Branch: `rfc-0026-web-terminal`. The RFC is already claimed (`38032d2`).
- Commit messages follow Conventional Commits, as the repo's commitlint requires: `feat(api):`, `feat(ui):`, `refactor(kexec):`, `docs(rfcs):`.
- Every commit message ends with these two lines:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp
  ```
- Go tests: `go test ./...`. UI lint: `cd ui && npm run lint`. Full lint: `make lint`.
- Idle timeout is exactly `30 * time.Minute`; ticket TTL is exactly `30 * time.Second`.
- One shell per user per project, enforced in memory — valid only while the server is `replicas: 1` (`deploy/components/shpyrd/base/server.yaml:8`). Every place that relies on it carries a comment saying so.
- The app container is named `"app"` (literal, as in `internal/cli/shell.go:72` and `internal/controller/desired.go:308`).
- Shell fallback order, unchanged from `internal/cli/shell.go:146`: `/cnb/lifecycle/launcher -- bash`, `bash`, `/cnb/lifecycle/launcher -- sh`, `sh`.
- Comment style: explain *why*, in prose, matching the density of the surrounding files. No comment that merely restates the code.

## Review Focus

These are the failure modes the spec implies but that no task's happy path exercises. Each has its test assigned to the task that owns the code.

1. **An instance vanishes between the selector loading and the connect** (routine during a rolling deploy) — expect a 404 naming the instance before any upgrade, not a terminal stuck on "Connecting". → Task 5.
2. **The image has no shell at all** (distroless, `scratch`) — expect an `error` frame carrying "no usable shell in the image" rendered in the terminal, not a silent close. → Task 5.
3. **Binary or non-UTF-8 output** (`cat /bin/ls`, a `\xff` byte) — expect bytes to survive the bridge unchanged; a `string()` round trip or a `TextDecoder` on the client would corrupt them. → Task 5 (server) and Task 6 (client).
4. **A grant revoked inside the ticket's 30-second window** — expect the connect to be refused, because roles are re-resolved at redemption rather than trusted from mint time. → Task 5.
5. **A resize frame arriving after the process has exited** — expect it to be dropped, not to panic writing to a closed channel. → Task 5.

---

### Task 1: Extract `kexec.StreamIO`

`kexec.Stream` hardcodes `os.Stdin`, `os.Stderr` and local raw mode, so the server cannot use it. Pull out the executor construction and add a terminal-free variant driven by a channel of sizes.

**Files:**
- Modify: `pkg/kexec/kexec.go:110-137` (the `Stream` function) and the `sizeQueue` section at `pkg/kexec/kexec.go:160-200`
- Test: `pkg/kexec/kexec_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func StreamIO(ctx context.Context, k *kube.Client, rawURL string, tty bool, stdin io.Reader, stdout, stderr io.Writer, sizes <-chan remotecommand.TerminalSize) error`
  - `func ExecURL(k *kube.Client, namespace, pod, container string, command []string, tty bool) string`
  - unchanged: `Run`, `IsNotFound`, `RemoteExit`, `ExitCode`

- [ ] **Step 1: Write the failing test**

Append to `pkg/kexec/kexec_test.go`:

```go
func TestChanSizeQueue(t *testing.T) {
	ch := make(chan remotecommand.TerminalSize, 1)
	q := &chanSizeQueue{ch: ch}
	ch <- remotecommand.TerminalSize{Width: 120, Height: 40}
	got := q.Next()
	if got == nil || got.Width != 120 || got.Height != 40 {
		t.Fatalf("Next() = %+v, want 120x40", got)
	}
	// A closed channel ends the queue: remotecommand stops its size
	// goroutine on nil, which is how the server avoids leaking one per
	// finished session.
	close(ch)
	if q.Next() != nil {
		t.Error("Next() after close must be nil so the size goroutine exits")
	}
}

func TestExecURL(t *testing.T) {
	k := &kube.Client{Kube: kubefake.NewSimpleClientset()}
	u := ExecURL(k, "app-blog", "blog-web-1", "app", []string{"bash"}, true)
	for _, want := range []string{"/namespaces/app-blog/pods/blog-web-1/exec", "container=app", "command=bash", "tty=true", "stdin=true"} {
		if !strings.Contains(u, want) {
			t.Errorf("ExecURL() = %q, missing %q", u, want)
		}
	}
	// With a TTY the remote side must not get a separate stderr stream:
	// there is only one terminal to write to.
	if strings.Contains(u, "stderr=true") {
		t.Errorf("ExecURL() with tty must not request stderr: %q", u)
	}
}
```

Add to that file's imports: `"strings"`, `"k8s.io/client-go/tools/remotecommand"`, `kubefake "k8s.io/client-go/kubernetes/fake"`, `"shpyrd/pkg/kube"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/kexec/ -run 'TestChanSizeQueue|TestExecURL' -v`
Expected: FAIL — `undefined: chanSizeQueue`, `undefined: ExecURL`.

- [ ] **Step 3: Write minimal implementation**

In `pkg/kexec/kexec.go`, add `ExecURL`, refactor the executor construction out of `Stream`, and add `StreamIO` plus `chanSizeQueue`:

```go
// ExecURL is the API URL of an exec stream into a container. Callers that
// bridge the stream themselves (the server's web terminal) need the URL
// without the local terminal handling Exec applies.
func ExecURL(k *kube.Client, namespace, pod, container string, command []string, tty bool) string {
	req := k.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: command, Stdin: true, Stdout: true, Stderr: !tty, TTY: tty,
		}, scheme.ParameterCodec)
	return req.URL().String()
}

// newExecutor builds the executor both stream entry points use: WebSocket
// first, SPDY when the apiserver or a proxy in between cannot upgrade.
func newExecutor(k *kube.Client, rawURL string) (remotecommand.Executor, error) {
	ws, err := remotecommand.NewWebSocketExecutor(k.Config, "GET", rawURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	spdy, err := remotecommand.NewSPDYExecutor(k.Config, "POST", u)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool { return httpstream.IsUpgradeFailure(err) })
}

// StreamIO is Stream without a local terminal: explicit streams and sizes
// from a channel, so a server can bridge a browser socket into a pod. The
// caller closes sizes when the session ends, which stops the size goroutine
// remotecommand runs.
func StreamIO(ctx context.Context, k *kube.Client, rawURL string, tty bool, stdin io.Reader, stdout, stderr io.Writer, sizes <-chan remotecommand.TerminalSize) error {
	executor, err := newExecutor(k, rawURL)
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Tty: tty}
	if tty && sizes != nil {
		opts.TerminalSizeQueue = &chanSizeQueue{ch: sizes}
	}
	return executor.StreamWithContext(ctx, opts)
}

// chanSizeQueue feeds remotecommand from a channel instead of SIGWINCH.
type chanSizeQueue struct{ ch <-chan remotecommand.TerminalSize }

func (q *chanSizeQueue) Next() *remotecommand.TerminalSize {
	s, ok := <-q.ch
	if !ok {
		return nil
	}
	return &s
}
```

Then reduce `Stream` to reuse `newExecutor`, keeping its terminal handling:

```go
// Stream connects the local stdin/stdout to an exec or attach URL.
func Stream(ctx context.Context, k *kube.Client, rawURL string, tty bool, stdout io.Writer) error {
	executor, err := newExecutor(k, rawURL)
	if err != nil {
		return err
	}
	opts := remotecommand.StreamOptions{Stdin: os.Stdin, Stdout: stdout, Stderr: os.Stderr, Tty: tty}
	if tty {
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			defer term.Restore(int(os.Stdin.Fd()), state)
		}
		q := newSizeQueue()
		defer q.stop()
		opts.TerminalSizeQueue = q
	}
	return executor.StreamWithContext(ctx, opts)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/kexec/ -v && go build ./...`
Expected: PASS, and the CLI still compiles (`Exec`, `Attach`, `Run` unchanged).

- [ ] **Step 5: Commit**

```bash
git add pkg/kexec/kexec.go pkg/kexec/kexec_test.go
git commit -m "refactor(kexec): StreamIO, a stream primitive without a local terminal

Stream hardcoded os.Stdin, os.Stderr and raw mode, so only the CLI could use
it. Pull the WebSocket-with-SPDY-fallback executor into newExecutor and add
StreamIO, which takes explicit streams and terminal sizes from a channel, so
the server can bridge a browser socket into a pod over the same path.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 2: The instances endpoint

The selector needs instance names, and nothing serves them today. Names come from `logs.InstanceNames` so they match the log viewer, and run pods are excluded per the RFC's non-goals.

**Files:**
- Create: `pkg/api/shell.go`
- Create: `pkg/api/shell_test.go`
- Modify: `pkg/api/server.go` (add one route beside the other project routes, around line 366)

**Interfaces:**
- Consumes: `s.loadApp`, `abort`, `s.require`, `logs.InstanceNames`, `shpyrdv1.LabelApp`, `shpyrdv1.LabelProcess`.
- Produces:
  - `type Instance struct { Name, Process, Pod string; Ready bool }` with JSON tags `name`, `process`, `pod`, `ready`
  - `func (s *Server) listInstances(c *gin.Context)`
  - `const runProcess = "run"`
  - `const appContainer = "app"`

- [ ] **Step 1: Write the failing test**

Create `pkg/api/shell_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// appPod is a pod of a project as the controller labels them.
func appPod(app, process, name string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "app-" + app,
			Labels:    map[string]string{shpyrdv1.LabelApp: app, shpyrdv1.LabelProcess: process},
		},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
}

func TestListInstances(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog},
		appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true),
		appPod("blog", "web", "blog-web-bbb", corev1.PodRunning, false),
		appPod("blog", "worker", "blog-worker-ccc", corev1.PodRunning, true),
		// A one-off command's pod (RFC-0024) shares the namespace and must
		// never be offered a shell.
		appPod("blog", runProcess, "blog-run-ddd", corev1.PodRunning, true),
		// Not yet running: no process to attach to.
		appPod("blog", "web", "blog-web-eee", corev1.PodPending, false),
	)

	rec := do(t, s, "GET", "/api/projects/blog/instances", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("instances: %d %s", rec.Code, rec.Body.String())
	}
	var got []Instance
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range got {
		names = append(names, i.Name)
	}
	if len(names) != 3 || names[0] != "web.1" || names[1] != "web.2" || names[2] != "worker.1" {
		t.Fatalf("instances = %v, want [web.1 web.2 worker.1]", names)
	}
	if got[0].Pod != "blog-web-aaa" || !got[0].Ready || got[1].Ready {
		t.Errorf("readiness or pod wrong: %+v", got[:2])
	}
	if got[2].Process != "worker" {
		t.Errorf("process = %q, want worker", got[2].Process)
	}
}

func TestListInstancesNeedsExec(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))
	s.authz.TTL = 1

	// Enforcement starts with the first team; then a viewer may read the
	// project but may not look for somewhere to run commands.
	if rec := do(t, s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/blog/members", `{"role":"viewer","user":"viewer@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add viewer: %d %s", rec.Code, rec.Body.String())
	}
	sid, _ := signIn(t, s, ext.Identity{Subject: "u2", Email: "viewer@example.test", Provider: "local"})
	if rec := doCookie(t, s, "GET", "/api/projects/blog/instances", "", sid, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer instances: %d, want 403", rec.Code)
	}
}
```

Add `"shpyrd/pkg/ext"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/api/ -run TestListInstances -v`
Expected: FAIL — `undefined: Instance`, `undefined: runProcess`.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/api/shell.go`:

```go
// The web terminal (RFC-0026): a shell into a running instance from the
// dashboard. The browser opens a WebSocket, the server bridges it to
// pods/exec with a TTY, and the `project.exec` role gates the whole feature.
package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/logs"
)

// runProcess is the process label of one-off pods (RFC-0024), which share the
// project's namespace and are not shellable. The controller keeps its own
// unexported copy; a third caller should move this to api/v1alpha1 rather
// than copy it again.
const runProcess = "run"

// appContainer is the container a project's process runs in.
const appContainer = "app"

// Instance is a running instance of a project, named the way the log viewer
// names it.
type Instance struct {
	Name    string `json:"name"`    // web.1
	Process string `json:"process"` // web
	Pod     string `json:"pod"`
	Ready   bool   `json:"ready"`
}

// listInstances lists what a shell can attach to: running app pods, never
// build or run pods (RFC-0026 non-goals).
func (s *Server) listInstances(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	out, err := s.instancesOf(c, app.Namespace, app.Name)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// instancesOf lists a project's shellable instances. Names are computed over
// the filtered set, which is safe because InstanceNames numbers within a
// process: dropping run pods cannot renumber web.1.
func (s *Server) instancesOf(c *gin.Context, namespace, app string) ([]Instance, error) {
	pods, err := s.kube.Kube.CoreV1().Pods(namespace).List(c.Request.Context(), metav1.ListOptions{
		// "!=" also matches pods carrying no process label at all.
		LabelSelector: shpyrdv1.LabelApp + "=" + app + "," + shpyrdv1.LabelProcess + "!=" + runProcess,
	})
	if err != nil {
		return nil, err
	}
	var running []corev1.Pod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	names := logs.InstanceNames(running)
	out := make([]Instance, 0, len(running))
	for _, p := range running {
		out = append(out, Instance{
			Name:    names[p.Name],
			Process: p.Labels[shpyrdv1.LabelProcess],
			Pod:     p.Name,
			Ready:   podReady(p),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
```

In `pkg/api/server.go`, beside the other project routes (after the audit route at line 366):

```go
	api.GET("/projects/:slug/instances", s.require(authz.ProjectExec), s.listInstances) // RFC-0026
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/api/ -run TestListInstances -v`
Expected: PASS both tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/api/shell.go pkg/api/shell_test.go pkg/api/server.go
git commit -m "feat(api): list a project's shellable instances (RFC-0026)

The Shell tab's selector needs instance names and nothing served them:
InstanceNames was only ever applied to log lines. Names come from the same
function so the two agree, and run pods are filtered out by label because
they share the project's namespace and are not shellable.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 3: Exec tickets and the one-shell registry

Two small in-memory units, tested without any HTTP. The ticket carries the actor because the WebSocket has no other credential to offer.

**Files:**
- Create: `pkg/api/shellstate.go`
- Create: `pkg/api/shellstate_test.go`

**Interfaces:**
- Consumes: `ext.Identity`.
- Produces:
  - `type execTicket struct { Identity ext.Identity; Project, Instance string; expires time.Time }`
  - `type ticketStore struct { … }` with `newTicketStore(ttl time.Duration) *ticketStore`, `(*ticketStore).mint(execTicket) (string, error)`, `(*ticketStore).redeem(code string) (execTicket, error)`, and a swappable `now func() time.Time` field
  - `type shellRegistry struct { … }` with `newShellRegistry() *shellRegistry`, `(*shellRegistry).claim(actor, project string) bool`, `(*shellRegistry).release(actor, project string)`, `(*shellRegistry).held(actor, project string) bool`
  - `func actorKey(id ext.Identity) string`
  - `const execTicketTTL = 30 * time.Second`

- [ ] **Step 1: Write the failing test**

Create `pkg/api/shellstate_test.go`:

```go
package api

import (
	"testing"
	"time"

	"shpyrd/pkg/ext"
)

func TestExecTicketOneShot(t *testing.T) {
	st := newTicketStore(execTicketTTL)
	id := ext.Identity{Subject: "u1", Email: "dev@example.test", Provider: "local"}
	code, err := st.mint(execTicket{Identity: id, Project: "blog", Instance: "web.1"})
	if err != nil {
		t.Fatal(err)
	}
	if code == "" {
		t.Fatal("mint returned an empty code")
	}
	got, err := st.redeem(code)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got.Project != "blog" || got.Instance != "web.1" || got.Identity.Email != "dev@example.test" {
		t.Errorf("redeemed = %+v", got)
	}
	// Replay is what the one-time code exists to stop: a second tab must not
	// be able to reuse a code it saw.
	if _, err := st.redeem(code); err == nil {
		t.Error("a redeemed ticket must not be redeemable again")
	}
}

func TestExecTicketExpires(t *testing.T) {
	st := newTicketStore(execTicketTTL)
	now := time.Now()
	st.now = func() time.Time { return now }
	code, err := st.mint(execTicket{Project: "blog", Instance: "web.1"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(execTicketTTL + time.Second)
	if _, err := st.redeem(code); err == nil {
		t.Error("an expired ticket must be refused")
	}
}

func TestExecTicketUnknownCode(t *testing.T) {
	st := newTicketStore(execTicketTTL)
	if _, err := st.redeem("not-a-ticket"); err == nil {
		t.Error("an unknown code must be refused")
	}
	if _, err := st.redeem(""); err == nil {
		t.Error("an empty code must be refused")
	}
}

func TestShellRegistryOnePerUserPerProject(t *testing.T) {
	r := newShellRegistry()
	if !r.claim("u1", "blog") {
		t.Fatal("the first claim must succeed")
	}
	if r.claim("u1", "blog") {
		t.Error("a second shell for the same user and project must be refused")
	}
	// The limit is per user and per project, not global.
	if !r.claim("u2", "blog") || !r.claim("u1", "shop") {
		t.Error("other users and other projects are unaffected")
	}
	if !r.held("u1", "blog") {
		t.Error("held must report a live shell")
	}
	r.release("u1", "blog")
	if r.held("u1", "blog") {
		t.Error("held must be false after release")
	}
	if !r.claim("u1", "blog") {
		t.Error("the slot must be reusable once released")
	}
}

func TestActorKeyDistinguishesProviders(t *testing.T) {
	a := actorKey(ext.Identity{Subject: "u1", Provider: "local"})
	b := actorKey(ext.Identity{Subject: "u1", Provider: "github"})
	if a == b {
		t.Error("the same subject from two providers is two different people")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/api/ -run 'TestExecTicket|TestShellRegistry|TestActorKey' -v`
Expected: FAIL — `undefined: newTicketStore`, `undefined: newShellRegistry`.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/api/shellstate.go`:

```go
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"shpyrd/pkg/ext"
)

// State behind the web terminal (RFC-0026): one-time tickets that
// authenticate a WebSocket, and the registry that holds a user to one shell
// per project.

// execTicketTTL is how long a minted ticket may be redeemed. It is short
// because the ticket is a bearer credential: the WebSocket presents nothing
// else.
const execTicketTTL = 30 * time.Second

// execTicket is what redeeming a code proves. It carries the actor because a
// browser cannot set headers on a WebSocket, and `shpyrd cluster dashboard`
// signs in with a token in localStorage and so has no session cookie either.
type execTicket struct {
	Identity ext.Identity
	Project  string
	Instance string
	expires  time.Time
}

// ticketStore holds unredeemed tickets. In memory is enough: they live for
// 30 seconds and the server runs a single replica
// (deploy/components/shpyrd/base/server.yaml).
type ticketStore struct {
	mu  sync.Mutex
	m   map[string]execTicket // keyed by the code's hash
	ttl time.Duration
	now func() time.Time
}

func newTicketStore(ttl time.Duration) *ticketStore {
	return &ticketStore{m: map[string]execTicket{}, ttl: ttl, now: time.Now}
}

// mint stores t and returns the code to put in the WebSocket's query. Only
// the hash is kept, so a heap dump or an accidental log of the store does not
// hand out working tickets.
func (st *ticketStore) mint(t execTicket) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
	t.expires = st.now().Add(st.ttl)
	st.m[hashCode(code)] = t
	return code, nil
}

// redeem consumes the ticket for code. One shot: gone whether it was valid or
// merely stale.
func (st *ticketStore) redeem(code string) (execTicket, error) {
	if code == "" {
		return execTicket{}, errors.New("no shell ticket presented")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
	key := hashCode(code)
	t, ok := st.m[key]
	delete(st.m, key)
	if !ok || st.now().After(t.expires) {
		return execTicket{}, errors.New("the shell ticket is invalid or expired; open the Shell tab again")
	}
	return t, nil
}

// sweepLocked drops expired tickets so an unredeemed one cannot accumulate.
func (st *ticketStore) sweepLocked() {
	now := st.now()
	for k, t := range st.m {
		if now.After(t.expires) {
			delete(st.m, k)
		}
	}
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// shellRegistry enforces one shell per user per project (RFC-0026). In memory
// is correct only while the server runs a single replica
// (deploy/components/shpyrd/base/server.yaml); scaling out moves this into
// the control-plane store.
type shellRegistry struct {
	mu   sync.Mutex
	live map[string]struct{}
}

func newShellRegistry() *shellRegistry {
	return &shellRegistry{live: map[string]struct{}{}}
}

func shellKey(actor, project string) string { return actor + "\x00" + project }

// claim takes the slot, reporting false when a shell is already live.
func (r *shellRegistry) claim(actor, project string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := shellKey(actor, project)
	if _, ok := r.live[k]; ok {
		return false
	}
	r.live[k] = struct{}{}
	return true
}

func (r *shellRegistry) release(actor, project string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, shellKey(actor, project))
}

func (r *shellRegistry) held(actor, project string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.live[shellKey(actor, project)]
	return ok
}

// actorKey identifies the person the limit applies to. Subject alone is not
// enough: two providers can issue the same one.
func actorKey(id ext.Identity) string { return id.Provider + "\x00" + id.Subject }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/api/ -run 'TestExecTicket|TestShellRegistry|TestActorKey' -v`
Expected: PASS all five.

- [ ] **Step 5: Commit**

```bash
git add pkg/api/shellstate.go pkg/api/shellstate_test.go
git commit -m "feat(api): one-time exec tickets and the one-shell-per-user registry

A WebSocket carries no header a browser can set, so the ticket is the
credential and therefore records the actor, hashes the code, lives 30 seconds
and redeems exactly once. The registry holds a user to one shell per project;
it is in memory, which is correct only while the server is single-replica.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 4: The ticket endpoint

Wire the store to a route: `project.exec`, the instance must exist, and a user who already has a shell is refused here rather than after the upgrade.

**Files:**
- Modify: `pkg/api/shell.go` (add the handler)
- Modify: `pkg/api/server.go` (one route)
- Modify: `pkg/api/server.go:100-125` (the `Server` struct) and `newServer` (initialise the two stores)
- Test: `pkg/api/shell_test.go`

**Interfaces:**
- Consumes: `ticketStore`, `shellRegistry`, `actorKey`, `s.instancesOf`, `ext.IdentityFrom`.
- Produces:
  - `func (s *Server) mintShellTicket(c *gin.Context)`
  - `Server` fields `execTickets *ticketStore` and `shells *shellRegistry`

- [ ] **Step 1: Write the failing test**

Append to `pkg/api/shell_test.go`:

```go
func TestMintShellTicket(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))

	rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var body struct{ Ticket string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ticket == "" {
		t.Fatal("no ticket in the response")
	}

	// An instance that is not there cannot be given a ticket: the terminal
	// would otherwise open and then fail with nothing useful to say.
	if rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.9", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance: %d, want 404", rec.Code)
	}
	if rec := do(t, s, "POST", "/api/projects/blog/shell/ticket", "", true); rec.Code != http.StatusBadRequest {
		t.Errorf("missing instance: %d, want 400", rec.Code)
	}
}

func TestMintShellTicketRefusesSecondShell(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))

	// A shell is already live for this caller: the admin token's identity.
	s.shells.claim(actorKey(ext.Identity{Subject: "admin-token", Provider: "token"}), "blog")
	rec := do(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", true)
	if rec.Code != http.StatusConflict {
		t.Errorf("second shell: %d, want 409", rec.Code)
	}
}

func TestMintShellTicketNeedsExec(t *testing.T) {
	blog := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: "app-blog"}}
	s, _ := newTestServer(t, nil, []client.Object{blog}, appPod("blog", "web", "blog-web-aaa", corev1.PodRunning, true))
	s.authz.TTL = 1
	if rec := do(t, s, "POST", "/api/teams", `{"name":"ops","members":["ops@example.test"],"platformRole":"platform-admin"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/blog/members", `{"role":"viewer","user":"viewer@example.test"}`, true); rec.Code != http.StatusCreated {
		t.Fatalf("add viewer: %d %s", rec.Code, rec.Body.String())
	}
	sid, csrf := signIn(t, s, ext.Identity{Subject: "u2", Email: "viewer@example.test", Provider: "local"})
	if rec := doCookie(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", sid, csrf); rec.Code != http.StatusForbidden {
		t.Errorf("viewer mint: %d, want 403", rec.Code)
	}
	// Being a POST, it is also a CSRF-protected route for cookie callers.
	if rec := doCookie(t, s, "POST", "/api/projects/blog/shell/ticket?instance=web.1", "", sid, ""); rec.Code == http.StatusOK {
		t.Error("a cookie caller without the CSRF header must not mint a ticket")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/api/ -run TestMintShellTicket -v`
Expected: FAIL — 404 from gin (no such route) and `s.shells` undefined.

- [ ] **Step 3: Write minimal implementation**

In `pkg/api/server.go`, add to the `Server` struct beside the other feature state:

```go
	// The web terminal (RFC-0026): unredeemed tickets and the live shells.
	execTickets *ticketStore
	shells      *shellRegistry
```

In `newServer`, where the other fields are initialised:

```go
	s.execTickets = newTicketStore(execTicketTTL)
	s.shells = newShellRegistry()
```

Add the route next to the instances route:

```go
	api.POST("/projects/:slug/shell/ticket", s.require(authz.ProjectExec), s.mintShellTicket) // RFC-0026
```

In `pkg/api/shell.go`:

```go
// mintShellTicket issues the one-time code the WebSocket presents. The
// instance is checked here so a terminal never opens against something that
// is not running, and the one-shell limit is refused here so a stale tab
// learns why before it dials.
func (s *Server) mintShellTicket(c *gin.Context) {
	instance := c.Query("instance")
	if instance == "" {
		abort(c, http.StatusBadRequest, errors.New("name the instance to open a shell on (?instance=web.1)"))
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
	if !hasInstance(instances, instance) {
		abort(c, http.StatusNotFound, fmt.Errorf("instance %q is not running; running: %s", instance, instanceList(instances)))
		return
	}
	id, _ := ext.IdentityFrom(c)
	if s.shells.held(actorKey(id), app.Name) {
		abort(c, http.StatusConflict, errors.New("you already have a shell open on this project; close it first"))
		return
	}
	code, err := s.execTickets.mint(execTicket{Identity: id, Project: app.Name, Instance: instance})
	if err != nil {
		abort(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ticket": code})
}

func hasInstance(list []Instance, name string) bool {
	for _, i := range list {
		if i.Name == name {
			return true
		}
	}
	return false
}

func instanceList(list []Instance) string {
	names := make([]string, 0, len(list))
	for _, i := range list {
		names = append(names, i.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
```

Add `"errors"`, `"fmt"`, `"strings"` and `"shpyrd/pkg/ext"` to that file's imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/api/ -run 'TestMintShellTicket|TestListInstances' -v`
Expected: PASS all.

- [ ] **Step 5: Commit**

```bash
git add pkg/api/shell.go pkg/api/server.go pkg/api/shell_test.go
git commit -m "feat(api): mint one-time shell tickets (RFC-0026)

The instance is verified and the one-shell limit refused at mint time, so a
stale tab is told why before it dials rather than after the upgrade. Being a
POST, cookie callers already have to prove intent with the CSRF header.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 5: The WebSocket bridge

The core. Exec is reached through injected seams so everything except the real pod stream is tested here. This task owns all five Review Focus items.

**Files:**
- Create: `pkg/api/shellbridge.go`
- Modify: `pkg/api/server.go` (the `Server` struct, `newServer` defaults, one route)
- Modify: `go.mod`, `go.sum` (promote `gorilla/websocket` to direct)
- Test: `pkg/api/shellbridge_test.go`

**Interfaces:**
- Consumes: `execTicket`, `ticketStore`, `shellRegistry`, `actorKey`, `s.instancesOf`, `s.audit`, `appContainer`, `kexec.StreamIO`, `kexec.ExecURL`, `kexec.Run`, `kexec.IsNotFound`, `kexec.ExitCode`, `kexec.RemoteExit`.
- Produces:
  - `type execStreamFunc func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes <-chan remotecommand.TerminalSize) error`
  - `type probeShellFunc func(ctx context.Context, namespace, pod, container string) ([]string, error)`
  - `Server` fields `execStream execStreamFunc`, `probeShell probeShellFunc`, `shellIdle time.Duration`
  - `func (s *Server) appShell(c *gin.Context)`
  - `func sameOrigin(r *http.Request) bool`
  - `type wsConn struct { … }` with `Write([]byte) (int, error)`, `control(shellControl) error`, `close(code int, reason string) error`
  - `var errNoShell error`
  - `const shellIdleTimeout = 30 * time.Minute`
  - `var shellCandidates = [][]string{…}`

- [ ] **Step 1: Write the failing test**

Create `pkg/api/shellbridge_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the shell slot was never released")
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
```

The exit-code test builds the error with `kexec.ExitError` directly: it is exported and is exactly what `kexec.ExitCode` reads, so no fake client-go error shape is needed.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/api/ -run TestShell -v`
Expected: FAIL — `s.probeShell` undefined, no `/shell` route.

- [ ] **Step 3: Write minimal implementation**

Promote the dependency:

```bash
go get github.com/gorilla/websocket
go mod tidy
```

In `pkg/api/server.go`, add to the `Server` struct:

```go
	// The exec bridge (RFC-0026). The two function fields are the seam tests
	// replace: everything but the pod stream itself is then testable.
	execStream execStreamFunc
	probeShell probeShellFunc
	shellIdle  time.Duration
```

In `newServer`, after the stores:

```go
	s.execStream = s.streamExec
	s.probeShell = s.resolveShell
	s.shellIdle = shellIdleTimeout
```

And the route — deliberately **not** on a `require` chain, because the ticket is the credential and the role is checked inside the handler:

```go
	api.GET("/projects/:slug/shell", s.appShell) // RFC-0026; the ticket authorises, see appShell
```

Create `pkg/api/shellbridge.go`:

```go
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
		return
	}
	shell := command[len(command)-1]
	_ = w.control(shellControl{Type: "open", Instance: instance, Shell: shell})
	s.audit(c, project, "shell.open", instance, shell)

	// stdin is a pipe so the reader goroutine can hand bytes to the exec
	// stream, and closing it is how a closed socket ends the remote command.
	pr, pw := io.Pipe()
	sizes := make(chan remotecommand.TerminalSize, 4)
	idle := time.NewTimer(s.shellIdle)
	defer idle.Stop()

	// The reader goroutine is the only owner of sizes: it is the only sender,
	// so it is the only safe closer. Closing from here while the reader was
	// still alive would let a resize race into a closed channel, and a panic
	// in a bare goroutine is not caught by gin's Recovery — it would take the
	// server down. The reader always exits, because the handler's deferred
	// conn.Close unblocks its ReadMessage.
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
	go func() {
		select {
		case <-idle.C:
			_ = w.control(shellControl{Type: "error", Message: fmt.Sprintf("shell closed after %s idle", s.shellIdle)})
			cancel()
		case <-ctx.Done():
		}
	}()

	err = s.execStream(ctx, namespace, pod, appContainer, command, pr, w, sizes)

	code := 0
	detail := "exit 0"
	switch {
	case err == nil:
	case ctx.Err() != nil:
		detail = "closed"
		_ = w.close(websocket.CloseNormalClosure, "closed")
		s.audit(c, project, "shell.close", instance, detail)
		return
	default:
		code = kexec.ExitCode(kexec.RemoteExit(err))
		detail = fmt.Sprintf("exit %d", code)
	}
	_ = w.control(shellControl{Type: "exit", Code: &code})
	_ = w.close(websocket.CloseNormalClosure, detail)
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

func (w *wsConn) close(code int, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(reason) > 120 {
		reason = reason[:120] // a close reason is capped at 125 bytes
	}
	return w.c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}
```

Note for the implementer: `internal/cli/shell.go` already declares `cnbLauncher`. These are different packages, so both may declare it; the RFC records that the fallback order, not the constant, is the shared contract.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/api/ -run TestShell -v && go test ./... && make lint`
Expected: PASS. Fix any `go vet` complaints before committing.

- [ ] **Step 5: Commit**

```bash
git add pkg/api/shellbridge.go pkg/api/shellbridge_test.go pkg/api/server.go go.mod go.sum
git commit -m "feat(api): bridge a WebSocket to pods/exec for the web terminal (RFC-0026)

Binary frames carry terminal bytes so a program printing binary is not
mangled by a UTF-8 round trip; text frames carry resize, open, exit and
error. The shell is resolved by cheap non-TTY probes before the terminal
opens, so a missing bash never writes into it. Roles are re-resolved at
redemption rather than trusted from the ticket, the idle timer follows client
input only, and both open and close are audited with the instance.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 6: The terminal component

**Files:**
- Create: `ui/src/components/shell-view.tsx`
- Modify: `ui/src/lib/api.ts` (types, two calls, one URL helper)
- Modify: `ui/package.json` (two dependencies)

**Interfaces:**
- Consumes: `GET /instances`, `POST /shell/ticket`, `GET /shell` (WebSocket).
- Produces:
  - `export type Instance = { name: string; process: string; pod: string; ready: boolean }`
  - `api.instances(slug)`, `api.shellTicket(slug, instance)`
  - `export function shellSocketURL(slug: string, instance: string, ticket: string): string`
  - `export function ShellView({ slug }: { slug: string })`

- [ ] **Step 1: Add the dependencies**

```bash
cd ui && npm install @xterm/xterm@^5.5.0 @xterm/addon-fit@^0.10.0
```

- [ ] **Step 2: Extend the API client**

In `ui/src/lib/api.ts`, beside the other types:

```ts
/** A running instance of a project (RFC-0026). */
export type Instance = {
  name: string;
  process: string;
  pod: string;
  ready: boolean;
};
```

In the `api` object:

```ts
  instances: (slug: string) => request<Instance[]>(`${project(slug)}/instances`),
  /** A one-time code for a shell socket: browsers cannot set headers on a
   * WebSocket, so the ticket is what authenticates it. */
  shellTicket: (slug: string, instance: string) =>
    request<{ ticket: string }>(
      `${project(slug)}/shell/ticket?instance=${encodeURIComponent(instance)}`,
      { method: "POST" },
    ),
```

And after `apiStream`:

```ts
/** WebSocket URL of a shell session on an instance. */
export function shellSocketURL(
  slug: string,
  instance: string,
  ticket: string,
): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const q = `instance=${encodeURIComponent(instance)}&ticket=${encodeURIComponent(ticket)}`;
  return `${proto}//${location.host}${project(slug)}/shell?${q}`;
}
```

- [ ] **Step 3: Write the component**

Create `ui/src/components/shell-view.tsx`:

```tsx
import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { api, ApiError, shellSocketURL, type Instance } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

type Status =
  | { kind: "picking" }
  | { kind: "connecting"; instance: string }
  | { kind: "open"; instance: string; shell: string }
  | { kind: "closed"; instance: string; message: string };

/** Control frames from the server; binary frames are terminal bytes. */
type Control =
  | { type: "open"; instance: string; shell: string }
  | { type: "exit"; code: number }
  | { type: "error"; message: string };

/** A shell into a running instance (RFC-0026): xterm.js over a WebSocket the
 * server bridges to pods/exec. */
export function ShellView({ slug }: { slug: string }) {
  const instances = useQuery({
    queryKey: ["instances", slug],
    queryFn: () => api.instances(slug),
    refetchInterval: 15_000,
  });
  const [instance, setInstance] = useState<string>("");
  const [status, setStatus] = useState<Status>({ kind: "picking" });
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const ws = useRef<WebSocket | null>(null);

  // Default to the first ready instance once the list arrives.
  useEffect(() => {
    if (!instance && instances.data?.length) {
      setInstance((instances.data.find((i) => i.ready) ?? instances.data[0]).name);
    }
  }, [instances.data, instance]);

  // One terminal for the life of the tab; sessions come and go inside it.
  useEffect(() => {
    if (!host.current) return;
    const t = new Terminal({
      convertEol: false,
      cursorBlink: true,
      fontFamily:
        'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      theme: { background: "#09090b", foreground: "#f4f4f5" },
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(host.current);
    f.fit();
    term.current = t;
    fit.current = f;
    return () => {
      t.dispose();
      term.current = null;
      fit.current = null;
    };
  }, []);

  const sendResize = useCallback(() => {
    const t = term.current;
    const sock = ws.current;
    fit.current?.fit();
    if (!t || sock?.readyState !== WebSocket.OPEN) return;
    sock.send(
      JSON.stringify({ type: "resize", cols: t.cols, rows: t.rows }),
    );
  }, []);

  // Refit on container resize, debounced: dragging a window edge otherwise
  // sends a frame per pixel.
  useEffect(() => {
    if (!host.current) return;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const ro = new ResizeObserver(() => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(sendResize, 100);
    });
    ro.observe(host.current);
    return () => {
      ro.disconnect();
      if (timer) clearTimeout(timer);
    };
  }, [sendResize]);

  const connect = useCallback(async () => {
    const t = term.current;
    if (!t || !instance) return;
    ws.current?.close();
    t.reset();
    setStatus({ kind: "connecting", instance });
    t.writeln(`Connecting to ${instance}...`);
    let ticket: string;
    try {
      ticket = (await api.shellTicket(slug, instance)).ticket;
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : String(e);
      t.writeln(`\r\n${msg}`);
      setStatus({ kind: "closed", instance, message: msg });
      return;
    }
    const sock = new WebSocket(shellSocketURL(slug, instance, ticket));
    sock.binaryType = "arraybuffer";
    ws.current = sock;

    sock.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        let ctl: Control;
        try {
          ctl = JSON.parse(ev.data) as Control;
        } catch {
          return;
        }
        if (ctl.type === "open") {
          setStatus({ kind: "open", instance: ctl.instance, shell: ctl.shell });
          sendResize();
        } else if (ctl.type === "exit") {
          setStatus({
            kind: "closed",
            instance,
            message: `Process exited with status ${ctl.code}`,
          });
        } else if (ctl.type === "error") {
          t.writeln(`\r\n${ctl.message}`);
          setStatus({ kind: "closed", instance, message: ctl.message });
        }
        return;
      }
      // Bytes, not text: a Uint8Array keeps output a program prints intact,
      // and xterm decodes UTF-8 across chunk boundaries itself.
      t.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    sock.onclose = () => {
      if (ws.current === sock) ws.current = null;
      setStatus((prev) =>
        prev.kind === "closed"
          ? prev
          : { kind: "closed", instance, message: "Session ended" },
      );
    };
    sock.onerror = () => {
      t.writeln("\r\nThe connection failed.");
    };
    // Keystrokes go out as bytes for the same reason output comes back as
    // bytes.
    const enc = new TextEncoder();
    t.onData((d) => {
      if (sock.readyState === WebSocket.OPEN) sock.send(enc.encode(d));
    });
  }, [instance, sendResize, slug]);

  // Closing the socket on unmount is what ends the session when the tab is
  // closed or the user navigates away.
  useEffect(() => () => ws.current?.close(), []);

  const live = status.kind === "open" || status.kind === "connecting";
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={instance} onValueChange={setInstance} disabled={live}>
          <SelectTrigger className="w-56">
            <SelectValue placeholder="Choose an instance" />
          </SelectTrigger>
          <SelectContent>
            {(instances.data ?? []).map((i: Instance) => (
              <SelectItem key={i.name} value={i.name}>
                {i.name}
                {i.ready ? "" : " (not ready)"}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {live ? (
          <Button variant="outline" onClick={() => ws.current?.close()}>
            Disconnect
          </Button>
        ) : (
          <Button onClick={connect} disabled={!instance}>
            {status.kind === "closed" ? "Reconnect" : "Connect"}
          </Button>
        )}
        <span className="text-xs text-muted-foreground">
          {status.kind === "open"
            ? `${status.instance} · ${status.shell}`
            : status.kind === "closed"
              ? status.message
              : status.kind === "connecting"
                ? `Connecting to ${status.instance}...`
                : (instances.data?.length ?? 0) === 0
                  ? "No running instances"
                  : "One shell per project at a time; idle sessions close after 30 minutes."}
        </span>
      </div>
      <div
        ref={host}
        className="h-[30rem] overflow-hidden rounded-lg border bg-zinc-950 p-2"
      />
    </div>
  );
}
```

- [ ] **Step 4: Verify it builds and lints**

Run: `cd ui && npm run build && npm run lint`
Expected: both clean. `ui/src/components/ui/select.tsx` and `button.tsx` already exist, so no new UI primitive is needed — do not add one.

- [ ] **Step 5: Commit**

```bash
git add ui/src/components/shell-view.tsx ui/src/lib/api.ts ui/package.json ui/package-lock.json
git commit -m "feat(ui): xterm.js terminal for the web terminal (RFC-0026)

Output arrives as binary frames and is written as a Uint8Array so a program
printing binary is not corrupted, and keystrokes go out the same way. Resize
is debounced because dragging a window edge would otherwise send a frame per
pixel, and closing the socket on unmount is what ends the session when the
tab closes.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 7: The Shell tab

**Files:**
- Modify: `ui/src/pages/app-detail.tsx:176-198` (the `Tabs` block) and its imports

**Interfaces:**
- Consumes: `ShellView`, `usePerms().exec`.
- Produces: nothing other tasks use.

- [ ] **Step 1: Wire the tab**

At the top of `ui/src/pages/app-detail.tsx`, add to the React import and a lazy import:

```tsx
import { lazy, Suspense } from "react";

// xterm is only worth its bundle to someone who opens the tab, and the
// component must not mount before then: mounting dials a socket.
const ShellView = lazy(() =>
  import("@/components/shell-view").then((m) => ({ default: m.ShellView })),
);
```

In the `Tabs` block, after the Logs trigger:

```tsx
          {perms.exec && <TabsTrigger value="shell">Shell</TabsTrigger>}
```

and after the Logs content:

```tsx
        {perms.exec && (
          <TabsContent value="shell" className="pt-4">
            <Suspense
              fallback={
                <div className="text-sm text-muted-foreground">
                  Loading the terminal...
                </div>
              }
            >
              <ShellView slug={slug} />
            </Suspense>
          </TabsContent>
        )}
```

`usePerms` is already imported at `app-detail.tsx:36` and `perms` is already in scope at line 95, alongside `slug` — the `Tabs` block at line 176 can use both directly, and `perms.deploy`/`perms.destroy` at lines 165-169 are the pattern to copy. Passing just `slug` follows `DomainsCard slug={app.slug}` at line 667 rather than the `app={a}` the other tabs take, because the terminal needs nothing else from the App.

Radix unmounts inactive `TabsContent`, so no socket opens until the tab is selected and the socket closes when the user leaves it.

- [ ] **Step 2: Verify**

Run: `cd ui && npm run build && npm run lint`
Expected: clean, and the build output shows a separate chunk for xterm rather than one large bundle.

- [ ] **Step 3: Commit**

```bash
git add ui/src/pages/app-detail.tsx
git commit -m "feat(ui): Shell tab on the project page (RFC-0026)

Gated on project.exec and lazily loaded, so xterm stays out of the main
bundle and no socket opens until someone selects the tab.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

### Task 8: Verify against the cluster and close the RFC

Fakes cannot tell you whether xterm and a real `bash` agree about resize, whether the CNB launcher probe works on a buildpack image, or whether the CSP lets the socket open. This task is the real check.

**Files:**
- Modify: `rfcs/0026-web-terminal.md` (status, Implementation History)
- Modify: `rfcs/README.md` (index status)

- [ ] **Step 1: Build and deploy to the dev cluster**

```bash
cd /home/nkr/Projects/shpyrd && make dev-deploy
```

The cluster is named `shpyrd` on ports 8080/8443; the dashboard is `https://shpyrd.127.0.0.1.nip.io:8443`. If the cluster is gone, recreate it with the explicit ports (`./bin/shpyrd cluster create --name shpyrd --skip shpyrd --http-port 8080 --https-port 8443`), and ask the owner to run anything needing `sudo` — it prompts for a password.

- [ ] **Step 2: Walk the terminal through the checks**

In Chromium (which trusts via `~/.pki/nssdb`), open the `blog` example project's Shell tab and confirm each:

1. The selector lists the running instances with the same names the Logs tab shows.
2. Connecting opens a prompt; `echo $PATH` shows the buildpack environment, proving the launcher probe picked the right candidate.
3. `printf 'wide\n'` after dragging the window narrower and wider reflows; `stty size` reports the same size xterm shows.
4. `cat /bin/ls | head -c 64` prints binary without killing the terminal.
5. `exit` shows "Process exited with status 0"; a non-zero `exit 3` shows 3.
6. Opening the Shell tab in a second browser tab is refused with the one-shell message.
7. Leaving the tab and returning starts a fresh session; the Network panel shows no socket until the tab is selected.
8. The Console shows no CSP violation for the `wss:` connection.
9. `./bin/shpyrd projects audit blog` (or the project's Audit view) lists `shell.open` and `shell.close` with the actor and instance.

Record anything that fails as a fix in this task rather than moving on.

- [ ] **Step 3: Confirm the whole suite still passes**

```bash
go test ./... && make lint
```

- [ ] **Step 4: Close the RFC**

In `rfcs/0026-web-terminal.md` set the status and add the history entry:

```markdown
**Status:** implemented
```

```markdown
- 2026-09-26: implemented. Verified on the kind dev cluster against
  `examples/blog`: launcher environment, resize agreeing with `stty size`,
  binary output, exit status, the one-shell limit and the audit entries.
```

Set the Owner line to the branch or PR that carried it, and in `rfcs/README.md` change the index entry for 0026 to `implemented`.

- [ ] **Step 5: Commit**

```bash
git add rfcs/0026-web-terminal.md rfcs/README.md
git commit -m "docs(rfcs): RFC-0026 implemented

Verified on the kind dev cluster against examples/blog.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CFQobmze26mnGVmyUF8oVp"
```

---

## Notes for the reviewer

- `pkg/api/shell.go`, `shellstate.go` and `shellbridge.go` are three files rather than one because they fail differently: the listing is a Kubernetes query, the state is concurrency, the bridge is a protocol. `app-detail.tsx` gains only a tab; at 104KB it is already too large and this plan deliberately does not make it worse.
- The one real duplication introduced is `cnbLauncher` and the candidate list, which `internal/cli/shell.go` also has. The RFC records that the fallback *order* is the contract. If a third caller appears, move both into `pkg/kexec`.
- `runProcess = "run"` and `appContainer = "app"` are literals the controller and CLI also carry. Copying them twice is the existing practice; the comment in `shell.go` says what to do on the third.
