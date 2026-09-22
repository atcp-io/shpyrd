package install

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Variables are substituted into rendered manifests and Helm values as
// ${SHPYRD_NAME}. Only that exact form is touched so `$` in upstream
// manifests (nginx snippets, shell fragments) is left alone.
var varPattern = regexp.MustCompile(`\$\{(SHPYRD_[A-Z0-9_]+)\}`)

// Well known variables.
const (
	VarDomain       = "SHPYRD_DOMAIN"        // apps and dashboard domain, e.g. 127.0.0.1.nip.io
	VarCluster      = "SHPYRD_CLUSTER"       // cluster name
	VarProfile      = "SHPYRD_PROFILE"       // profile name
	VarRegistryHost = "SHPYRD_REGISTRY_HOST" // in-cluster registry host:port
	VarVersion      = "SHPYRD_VERSION"       // shpyrd version being installed
	VarSystemNS     = "SHPYRD_SYSTEM_NS"     // namespace of shpyrd's own components
	VarHTTPPort     = "SHPYRD_HTTP_PORT"     // host port reaching ingress HTTP (URLs only)
	VarHTTPSPort    = "SHPYRD_HTTPS_PORT"    // host port reaching ingress HTTPS (URLs only)
)

// BaseURL returns a function building https URLs for <name>.<domain>,
// including the port when it is not 443.
func BaseURL(vars map[string]string) func(name string) string {
	domain := vars[VarDomain]
	port := vars[VarHTTPSPort]
	return func(name string) string {
		u := "https://" + name + "." + domain
		if port != "" && port != "443" {
			u += ":" + port
		}
		return u
	}
}

// Substitute replaces variables in data. Unknown variables are an error so
// typos in manifests surface immediately.
func Substitute(data []byte, vars map[string]string) ([]byte, error) {
	var missing []string
	out := varPattern.ReplaceAllFunc(data, func(m []byte) []byte {
		name := string(m[2 : len(m)-1])
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return []byte(v)
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("undefined variables: %s", strings.Join(uniq(missing), ", "))
	}
	return out, nil
}

func uniq(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// mergeVars returns base overridden by extra.
func mergeVars(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
