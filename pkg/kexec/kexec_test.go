package kexec

import (
	"errors"
	"testing"

	kexec "k8s.io/client-go/util/exec"
)

func TestRemoteExit(t *testing.T) {
	err := RemoteExit(kexec.CodeExitError{Err: errors.New("command terminated with exit code 3"), Code: 3})
	if ExitCode(err) != 3 {
		t.Errorf("exit code = %d, want 3", ExitCode(err))
	}
	plain := errors.New("boom")
	if RemoteExit(plain) != plain || ExitCode(plain) != 1 {
		t.Error("other errors pass through and exit 1")
	}
	if RemoteExit(nil) != nil {
		t.Error("nil stays nil")
	}
}

func TestIsNotFound(t *testing.T) {
	for _, m := range []string{
		`OCI runtime exec failed: exec failed: unable to start container process: exec: "bash": executable file not found in $PATH: unknown`,
		"command terminated with exit code 126",
		"command terminated with exit code 127",
	} {
		if !IsNotFound(errors.New(m)) {
			t.Errorf("should be treated as missing executable: %s", m)
		}
	}
	if IsNotFound(errors.New("command terminated with exit code 2")) || IsNotFound(nil) {
		t.Error("ordinary failures are not missing executables")
	}
}
