package logfmt_test

import (
	"testing"

	"shpyrd/pkg/logfmt"
)

func TestParsePlainLine(t *testing.T) {
	e := logfmt.Parse("Listening on :8080")
	if e.Structured {
		t.Errorf("Structured = true, want false")
	}
	if e.Message != "Listening on :8080" {
		t.Errorf("Message = %q", e.Message)
	}
	if e.Level != logfmt.LevelInfo {
		t.Errorf("Level = %q, want info", e.Level)
	}
	if e.LevelText != "" {
		t.Errorf("LevelText = %q, want empty", e.LevelText)
	}
	if len(e.Fields) != 0 {
		t.Errorf("Fields = %v, want none", e.Fields)
	}
}

func TestParseLevelFromPlainText(t *testing.T) {
	for line, want := range map[string]logfmt.Level{
		"Listening on :8080":      logfmt.LevelInfo,
		"WARN disk almost full":   logfmt.LevelWarn,
		"panic: runtime error":    logfmt.LevelError,
		"request failed after 3s": logfmt.LevelError,
	} {
		if got := logfmt.Parse(line).Level; got != want {
			t.Errorf("Parse(%q).Level = %q, want %q", line, got, want)
		}
	}
}

func TestParseStructured(t *testing.T) {
	e := logfmt.Parse(`{"level":"info","msg":"request served","time":"2026-09-25T10:00:00Z"}`)
	if !e.Structured {
		t.Fatalf("Structured = false, want true")
	}
	if e.Level != logfmt.LevelInfo || e.LevelText != "info" {
		t.Errorf("Level = %q/%q", e.Level, e.LevelText)
	}
	if e.Message != "request served" {
		t.Errorf("Message = %q", e.Message)
	}
	if e.Time != "2026-09-25T10:00:00Z" {
		t.Errorf("Time = %q", e.Time)
	}
	if len(e.Fields) != 0 {
		t.Errorf("Fields = %v, want none", e.Fields)
	}
}

func TestParseKeepsFieldOrder(t *testing.T) {
	e := logfmt.Parse(`{"msg":"hi","zebra":1,"alpha":"two","level":"warn"}`)
	want := []logfmt.Field{{Key: "zebra", Value: "1"}, {Key: "alpha", Value: "two"}}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

func TestParseErrorFieldLeads(t *testing.T) {
	e := logfmt.Parse(`{"msg":"boom","a":"1","err":"connection reset"}`)
	want := []logfmt.Field{{Key: "error", Value: "connection reset"}, {Key: "a", Value: "1"}}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

func TestParseNestedValues(t *testing.T) {
	e := logfmt.Parse(`{"msg":"hi","req":{"path":"/","n":2},"tags":["a","b"],"ok":true,"none":null}`)
	want := []logfmt.Field{
		{Key: "req", Value: `{"path":"/","n":2}`},
		{Key: "tags", Value: `["a","b"]`},
		{Key: "ok", Value: "true"},
		{Key: "none", Value: "null"},
	}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

func TestParseAliases(t *testing.T) {
	e := logfmt.Parse(`{"severity":"ERROR","message":"nope","@timestamp":"2026-09-25T10:00:00Z","error":"eof"}`)
	if e.LevelText != "ERROR" || e.Level != logfmt.LevelError {
		t.Errorf("Level = %q/%q", e.Level, e.LevelText)
	}
	if e.Message != "nope" || e.Time != "2026-09-25T10:00:00Z" {
		t.Errorf("Message = %q Time = %q", e.Message, e.Time)
	}
	want := []logfmt.Field{{Key: "error", Value: "eof"}}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

func TestParseAliasesLvlEventTs(t *testing.T) {
	e := logfmt.Parse(`{"lvl":"debug","event":"tick","ts":1758790000}`)
	if e.Level != logfmt.LevelDebug || e.Message != "tick" || e.Time != "1758790000" {
		t.Errorf("got %+v", e)
	}
}

func TestParseFoldsLevelNames(t *testing.T) {
	for text, want := range map[string]logfmt.Level{
		"fatal":   logfmt.LevelError,
		"WARNING": logfmt.LevelWarn,
		"trace":   logfmt.LevelDebug,
		"notice":  logfmt.LevelInfo,
	} {
		if got := logfmt.Parse(`{"level":"` + text + `","msg":"x"}`).Level; got != want {
			t.Errorf("level %q = %q, want %q", text, got, want)
		}
	}
}

func TestParseGuessesLevelFromMessage(t *testing.T) {
	if got := logfmt.Parse(`{"msg":"failed to connect"}`).Level; got != logfmt.LevelError {
		t.Errorf("Level = %q, want error", got)
	}
}

func TestParseOnlyLooksStructured(t *testing.T) {
	for _, line := range []string{
		"[1,2,3]",
		`{"msg":"cut off"`,
		`"hello"`,
		`{"a":1} trailing`,
		"{}{}",
	} {
		if e := logfmt.Parse(line); e.Structured {
			t.Errorf("Parse(%q) reported structured", line)
		} else if e.Message != line {
			t.Errorf("Parse(%q).Message = %q, want the line itself", line, e.Message)
		}
	}
}

func TestParseIndentedObject(t *testing.T) {
	if !logfmt.Parse(`   {"msg":"hi"}  `).Structured {
		t.Errorf("indented object not parsed")
	}
}

func TestParseEmptyMessage(t *testing.T) {
	e := logfmt.Parse(`{"level":"info","user":"ana"}`)
	if e.Message != "" {
		t.Errorf("Message = %q, want empty", e.Message)
	}
	want := []logfmt.Field{{Key: "user", Value: "ana"}}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

func TestParseRepeatedKeyKeepsLastValueInPlace(t *testing.T) {
	e := logfmt.Parse(`{"msg":"hi","a":"1","b":"2","a":"3"}`)
	want := []logfmt.Field{{Key: "a", Value: "3"}, {Key: "b", Value: "2"}}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

func TestPretty(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`{"level":"info","msg":"request served","path":"/","ms":12}`, `INFO  request served path=/ ms=12`},
		{`{"level":"error","msg":"boom","err":"eof"}`, `ERROR boom error=eof`},
		{`{"msg":"no level here"}`, `no level here`},
		{"plain line", "plain line"},
		{`{"level":"warn","msg":"slow","note":"took a while"}`, `WARN  slow note="took a while"`},
		{`{"level":"info","user":"ana"}`, `INFO  user=ana`},
	} {
		if got := logfmt.Parse(tc.line).Pretty(); got != tc.want {
			t.Errorf("Parse(%q).Pretty() = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func equal(a, b []logfmt.Field) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Loggers occasionally emit two spellings of the same well-known key. Both
// are consumed, the first non-empty value wins, and neither shows up as a
// field: ui/src/lib/logs.ts behaves the same way.
func TestParseDuplicateWellKnownKeys(t *testing.T) {
	e := logfmt.Parse(`{"msg":"a","message":"b","level":"","severity":"warn","keep":"1"}`)
	if e.Message != "a" {
		t.Errorf("Message = %q, want a", e.Message)
	}
	if e.LevelText != "warn" || e.Level != logfmt.LevelWarn {
		t.Errorf("Level = %q/%q, want warn", e.Level, e.LevelText)
	}
	want := []logfmt.Field{{Key: "keep", Value: "1"}}
	if !equal(e.Fields, want) {
		t.Errorf("Fields = %v, want %v", e.Fields, want)
	}
}

// pino and the syslog severities report the level as a number. Showing "30"
// tells the reader nothing, so a numeric level is named by its bucket.
func TestParseNumericLevels(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  logfmt.Level
		text  string
	}{
		{"10", logfmt.LevelDebug, "debug"}, // pino trace
		{"20", logfmt.LevelDebug, "debug"}, // pino debug
		{"30", logfmt.LevelInfo, "info"},   // pino info
		{"40", logfmt.LevelWarn, "warn"},   // pino warn
		{"50", logfmt.LevelError, "error"}, // pino error
		{"60", logfmt.LevelError, "error"}, // pino fatal
		{"3", logfmt.LevelError, "error"},  // syslog err
		{"4", logfmt.LevelWarn, "warn"},    // syslog warning
		{"6", logfmt.LevelInfo, "info"},    // syslog info
		{"7", logfmt.LevelDebug, "debug"},  // syslog debug
	} {
		e := logfmt.Parse(`{"level":` + tc.level + `,"msg":"x"}`)
		if e.Level != tc.want || e.LevelText != tc.text {
			t.Errorf("level %s = %q/%q, want %q/%q", tc.level, e.Level, e.LevelText, tc.want, tc.text)
		}
	}
}

func TestPrettyDoesNotEscapeNestedValues(t *testing.T) {
	got := logfmt.Parse(`{"level":"error","msg":"job lost","job":{"id":"j-12","queue":"mail"},"tags":["a b"]}`).Pretty()
	want := `ERROR job lost job={"id":"j-12","queue":"mail"} tags=["a b"]`
	if got != want {
		t.Errorf("Pretty() = %q, want %q", got, want)
	}
}
