package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// consoleReporter prints installer progress as indented, prefixed lines.
// It is safe for concurrent use.
type consoleReporter struct {
	mu  sync.Mutex
	out io.Writer
}

func (r *consoleReporter) Runlevel(name string, comps []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "\n==> %s: %s\n", name, strings.Join(comps, ", "))
}

func (r *consoleReporter) Step(component, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "    %-16s %s\n", component, msg)
}

func (r *consoleReporter) Done(component string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "    %-16s ready (%s)\n", component, took.Round(time.Second))
}

func (r *consoleReporter) Failed(component string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "    %-16s FAILED: %v\n", component, err)
}
