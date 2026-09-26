// Package kexec runs commands in pods and attaches to them with the local
// terminal: WebSocket with SPDY fallback, raw mode and resize forwarding
// when stdin is a terminal, remote exit codes surfaced as ExitError.
package kexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"

	"shpyrd/pkg/kube"
)

// ExitError carries a remote exit code.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("command exited with code %d", e.Code) }

// ExitCode returns the exit code embedded in err, or 1.
func ExitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 1
}

// StdinIsTerminal reports whether the CLI runs interactively.
func StdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// IsNotFound reports exec failures caused by a missing executable.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "executable file not found") || strings.Contains(m, "no such file or directory") || strings.Contains(m, "exit code 127") || strings.Contains(m, "exit code 126")
}

// RemoteExit turns a remote non-zero exit into an ExitError.
func RemoteExit(err error) error {
	var ce kexec.CodeExitError
	if errors.As(err, &ce) {
		return &ExitError{Code: ce.Code}
	}
	return err
}

// Exec runs command in the container; with tty the local terminal is put
// in raw mode and resizes are forwarded.
func Exec(ctx context.Context, k *kube.Client, namespace, pod, container string, command []string, tty bool) error {
	req := k.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: command, Stdin: true, Stdout: true, Stderr: !tty, TTY: tty,
		}, scheme.ParameterCodec)
	return Stream(ctx, k, req.URL().String(), tty, os.Stdout)
}

// Run executes command in the container without a terminal and returns
// what it printed; for short, non-interactive commands issued by the
// server (a `sync` before a snapshot).
func Run(ctx context.Context, k *kube.Client, namespace, pod, container string, command []string) (string, error) {
	req := k.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: command, Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)
	u := req.URL()
	spdy, err := remotecommand.NewSPDYExecutor(k.Config, "POST", u)
	if err != nil {
		return "", err
	}
	ws, err := remotecommand.NewWebSocketExecutor(k.Config, "GET", u.String())
	if err != nil {
		return "", err
	}
	executor, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool { return httpstream.IsUpgradeFailure(err) })
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &out})
	return out.String(), err
}

// Attach streams a pod's main process.
func Attach(ctx context.Context, k *kube.Client, namespace, pod, container string, tty bool, stdout io.Writer) error {
	req := k.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: container, Stdin: true, Stdout: true, Stderr: !tty, TTY: tty,
		}, scheme.ParameterCodec)
	return Stream(ctx, k, req.URL().String(), tty, stdout)
}

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
type chanSizeQueue struct {
	ch <-chan remotecommand.TerminalSize
}

func (q *chanSizeQueue) Next() *remotecommand.TerminalSize {
	s, ok := <-q.ch
	if !ok {
		return nil
	}
	return &s
}

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

// CountingWriter counts what was streamed (to know whether output was lost
// to a race with a fast-exiting process).
type CountingWriter struct {
	W io.Writer
	N int
}

func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	c.N += n
	return n, err
}

// sizeQueue forwards terminal size changes to the remote side.
type sizeQueue struct {
	ch   chan remotecommand.TerminalSize
	done chan struct{}
}

func newSizeQueue() *sizeQueue {
	q := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 1), done: make(chan struct{})}
	q.push()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-sig:
				q.push()
			case <-q.done:
				return
			}
		}
	}()
	return q
}

func (q *sizeQueue) push() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	select {
	case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
	default:
	}
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case s := <-q.ch:
		return &s
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() { close(q.done) }
