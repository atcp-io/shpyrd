package cli

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// --yes answers every question, including destructive ones that default to
// no; without it a non-terminal stdin (tests, CI) takes the default.
func TestConfirmYesFlag(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if !confirm(cmd, true, "Delete everything?", false) {
		t.Error("--yes must answer a default-no question with yes")
	}
	if !confirm(cmd, true, "Use it?", true) {
		t.Error("--yes must answer a default-yes question with yes")
	}
	if confirm(cmd, false, "Delete everything?", false) {
		t.Error("non-terminal stdin must take the default (no)")
	}
	if !confirm(cmd, false, "Use it?", true) {
		t.Error("non-terminal stdin must take the default (yes)")
	}
}
