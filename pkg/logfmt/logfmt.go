// Package logfmt reads one log line and says what it contains: a level, a
// message, a timestamp and the remaining fields.
//
// Applications log JSON in production, which reads badly as a wall of
// {"level":"info","msg":...}. Parse turns such a line into an Entry so
// `shpyrd logs --pretty` can render it, and leaves anything that is not a
// JSON object as plain text. The dashboard does the same in
// ui/src/lib/logs.ts; the two are kept in step deliberately, so a change
// here belongs there too.
package logfmt

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Level is the severity bucket a line falls into, whatever the logger
// called it.
type Level string

const (
	LevelError Level = "error"
	LevelWarn  Level = "warn"
	LevelInfo  Level = "info"
	LevelDebug Level = "debug"
)

// Field is one key/value pair the line carried beyond the well-known keys.
type Field struct {
	Key   string
	Value string
}

// Entry is a parsed log line.
type Entry struct {
	// Structured is true when the line was a JSON object.
	Structured bool
	// Level is the severity bucket, from the level field or guessed from
	// the text.
	Level Level
	// LevelText is the level as the line spelled it; empty when the line
	// names none.
	LevelText string
	Message   string
	// Time is the line's own timestamp, when it carried one. The container
	// timestamp is a better column to sort by, so callers printing a time
	// of their own can ignore this.
	Time string
	// Fields are the remaining pairs in the order the line wrote them,
	// with any error field first.
	Fields []Field
}

// Well-known keys, in the spellings the common loggers use.
var (
	levelKeys   = []string{"level", "severity", "lvl", "log.level"}
	messageKeys = []string{"msg", "message", "event"}
	timeKeys    = []string{"time", "ts", "timestamp", "@timestamp"}
	errorKeys   = []string{"error", "err"}
)

var (
	errorWords = regexp.MustCompile(`\b(error|err|fatal|panic|exception|traceback|failed)\b`)
	warnWords  = regexp.MustCompile(`\b(warn|warning)\b`)
)

// Parse reads a single line. It never fails: a line that is not a JSON
// object comes back as an unstructured Entry whose Message is the line.
func Parse(line string) Entry {
	fields, ok := decodeObject(strings.TrimSpace(line))
	if !ok {
		return Entry{Level: guessLevel(line), Message: line}
	}
	e := Entry{Structured: true}
	rest := make([]Field, 0, len(fields))
	var errValue string
	// Every well-known key is consumed, whichever spelling it used, so a
	// logger writing both "msg" and "message" does not show one of them as
	// a field; the first non-empty value of each wins.
	for _, f := range fields {
		switch {
		case matches(f.Key, levelKeys):
			e.LevelText = firstSet(e.LevelText, f.Value)
		case matches(f.Key, messageKeys):
			e.Message = firstSet(e.Message, f.Value)
		case matches(f.Key, timeKeys):
			e.Time = firstSet(e.Time, f.Value)
		case matches(f.Key, errorKeys):
			errValue = firstSet(errValue, f.Value)
		default:
			rest = append(rest, f)
		}
	}
	// The error reads as part of the message, so it leads the fields.
	if errValue != "" {
		e.Fields = append(e.Fields, Field{Key: "error", Value: errValue})
	}
	e.Fields = append(e.Fields, rest...)
	switch {
	case e.LevelText == "":
		e.Level = guessLevel(e.Message)
	default:
		e.Level = NormalizeLevel(e.LevelText)
	}
	return e
}

// Pretty renders the level, message and fields of a line as one string:
// "LEVEL message key=value...". Callers print their own time and instance
// columns around it; a line without a level starts at the message, so
// plain output is unchanged.
func (e Entry) Pretty() string {
	var parts []string
	if e.LevelText != "" {
		// The bucket, not the spelling: logrus writes "warning" and pino a
		// number, and the column has to stay one width. Padded so messages
		// line up across levels.
		parts = append(parts, pad(strings.ToUpper(string(e.Level)), 5))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	for _, f := range e.Fields {
		parts = append(parts, f.Key+"="+quote(f.Value))
	}
	return strings.Join(parts, " ")
}

// NormalizeLevel folds a logger's level into a severity bucket, by name or
// by number: pino counts in tens up to 60, the syslog severities count down
// from 0, and both are common enough to read.
func NormalizeLevel(text string) Level {
	text = strings.TrimSpace(text)
	if n, err := strconv.Atoi(text); err == nil {
		return numericLevel(n)
	}
	switch strings.ToLower(text) {
	case "error", "err", "fatal", "crit", "critical", "panic", "alert", "emerg", "emergency":
		return LevelError
	case "warn", "warning":
		return LevelWarn
	case "debug", "trace":
		return LevelDebug
	default:
		return LevelInfo
	}
}

// numericLevel reads pino's scale (10 trace to 60 fatal) and, below 10, the
// syslog severities (0 emerg to 7 debug).
func numericLevel(n int) Level {
	if n < 10 {
		switch {
		case n <= 3:
			return LevelError
		case n == 4:
			return LevelWarn
		case n <= 6:
			return LevelInfo
		default:
			return LevelDebug
		}
	}
	switch {
	case n >= 50:
		return LevelError
	case n >= 40:
		return LevelWarn
	case n >= 30:
		return LevelInfo
	default:
		return LevelDebug
	}
}

// decodeObject reads a whole JSON object, keeping the source order of its
// keys; a repeated key keeps its first position and its last value, as
// JSON.parse does in the browser. Anything else (an array, a scalar, a
// truncated object, trailing content) is not a log record: ok is false.
func decodeObject(body string) ([]Field, bool) {
	if !strings.HasPrefix(body, "{") {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(body))
	if _, err := dec.Token(); err != nil { // the opening brace
		return nil, false
	}
	var fields []Field
	index := map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		value := renderValue(raw)
		if at, seen := index[key]; seen {
			fields[at].Value = value
			continue
		}
		index[key] = len(fields)
		fields = append(fields, Field{Key: key, Value: value})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, false
	}
	// A record is the whole line; "{} {}" or '{"a":1} x' is not one.
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

// renderValue is the one-line form of a JSON value: strings unquoted,
// everything else compact JSON.
func renderValue(raw json.RawMessage) string {
	// Only a JSON string is shown unquoted; null would also unmarshal into
	// a string, and reads better as "null".
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return string(raw)
	}
	return out.String()
}

// guessLevel reads a level out of a line that does not carry one.
func guessLevel(text string) Level {
	if len(text) > 200 {
		text = text[:200]
	}
	m := strings.ToLower(text)
	switch {
	case errorWords.MatchString(m):
		return LevelError
	case warnWords.MatchString(m):
		return LevelWarn
	default:
		return LevelInfo
	}
}

// firstSet keeps the value already found, or takes the new one.
func firstSet(have, next string) string {
	if have != "" {
		return have
	}
	return next
}

func matches(key string, keys []string) bool {
	for _, k := range keys {
		if key == k {
			return true
		}
	}
	return false
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// quote wraps a value in quotes when it would otherwise run into the next
// field. A nested object or array is left as it is: escaping the quotes it
// is made of turns it into a thicket.
func quote(v string) string {
	if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
		return v
	}
	if v == "" || strings.ContainsAny(v, " \t\"") {
		return strconv.Quote(v)
	}
	return v
}
