package cli

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestCheckPortsFree(t *testing.T) {
	// A port this test holds open is genuinely unavailable.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	taken := l.Addr().(*net.TCPAddr).Port

	err = checkPortsFree(taken)
	if err == nil {
		t.Fatalf("checkPortsFree(%d) = nil, want an error", taken)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(taken)) {
		t.Errorf("error %q does not name port %d", err, taken)
	}

	// A free port, and the zero value the flags use for "not set".
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	if err := checkPortsFree(port, 0); err != nil {
		t.Errorf("checkPortsFree(%d, 0) = %v, want nil", port, err)
	}
}
