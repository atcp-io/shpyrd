package cli

import "testing"

func TestPrettyLogs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pretty, asJSON bool
		terminal       bool
		want           bool
		wantErr        bool
	}{
		{name: "terminal by default", terminal: true, want: true},
		{name: "pipe by default", terminal: false, want: false},
		{name: "--pretty into a pipe", pretty: true, terminal: false, want: true},
		{name: "--json on a terminal", asJSON: true, terminal: true, want: false},
		{name: "both flags", pretty: true, asJSON: true, terminal: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prettyLogs(tc.pretty, tc.asJSON, tc.terminal)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("prettyLogs() error = nil, want one")
				}
				return
			}
			if err != nil {
				t.Fatalf("prettyLogs() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("prettyLogs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenderLogLine(t *testing.T) {
	for _, tc := range []struct{ name, line, want string }{
		{"json rendered", `{"level":"info","msg":"served","path":"/"}`, "INFO  served path=/"},
		{"plain untouched", "Listening on :8080", "Listening on :8080"},
		{"not an object untouched", `{"msg":"cut off"`, `{"msg":"cut off"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderLogLine(tc.line, true); got != tc.want {
				t.Errorf("renderLogLine(%q, true) = %q, want %q", tc.line, got, tc.want)
			}
			if got := renderLogLine(tc.line, false); got != tc.line {
				t.Errorf("renderLogLine(%q, false) = %q, want the line untouched", tc.line, got)
			}
		})
	}
}
