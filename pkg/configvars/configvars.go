// Package configvars manages an app's config vars: the keys of Secret
// <app>-env plus per-key metadata (when each was last set). Values are
// write-only from the product's point of view: nothing here ever returns
// them.
package configvars

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// AnnotationMeta on the Secret holds {"KEY": {"updatedAt": RFC3339}}.
const AnnotationMeta = "shpyrd.io/config-meta"

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Var is one config var without its value.
type Var struct {
	Name      string `json:"name"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type meta struct {
	UpdatedAt string `json:"updatedAt"`
}

// ValidateKey checks that k is a sane environment variable name.
func ValidateKey(k string) error {
	if !keyPattern.MatchString(k) {
		return fmt.Errorf("invalid config var name %q: use letters, digits and underscores, not starting with a digit", k)
	}
	return nil
}

// Apply sets and unsets keys on the Secret and records the change time.
func Apply(sec *corev1.Secret, set map[string]string, unset []string, now time.Time) error {
	if len(set) == 0 && len(unset) == 0 {
		return errors.New("nothing to change")
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	m := readMeta(sec)
	ts := now.UTC().Format(time.RFC3339)
	for k, v := range set {
		if err := ValidateKey(k); err != nil {
			return err
		}
		sec.Data[k] = []byte(v)
		m[k] = meta{UpdatedAt: ts}
	}
	for _, k := range unset {
		delete(sec.Data, k)
		delete(m, k)
	}
	writeMeta(sec, m)
	return nil
}

// List returns the config var names, sorted, with their metadata.
func List(sec *corev1.Secret) []Var {
	if sec == nil {
		return []Var{}
	}
	m := readMeta(sec)
	out := make([]Var, 0, len(sec.Data))
	for k := range sec.Data {
		out = append(out, Var{Name: k, UpdatedAt: m[k].UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ParseDotenv parses KEY=VALUE lines (.env style: comments, blank lines and
// surrounding quotes are handled) into a map.
func ParseDotenv(text string) (map[string]string, error) {
	out := map[string]string{}
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n+1)
		}
		k = strings.TrimSpace(k)
		if err := ValidateKey(k); err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, nil
}

func readMeta(sec *corev1.Secret) map[string]meta {
	m := map[string]meta{}
	if raw := sec.Annotations[AnnotationMeta]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func writeMeta(sec *corev1.Secret, m map[string]meta) {
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	if len(m) == 0 {
		delete(sec.Annotations, AnnotationMeta)
		return
	}
	b, _ := json.Marshal(m)
	sec.Annotations[AnnotationMeta] = string(b)
}
