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
	// Derived variables, computed by the engine (see derivedVars).
	VarExtensions   = "SHPYRD_EXTENSIONS"    // enabled extensions, comma separated
	VarDashboardURL = "SHPYRD_DASHBOARD_URL" // external dashboard URL
	VarAuthURL      = "SHPYRD_AUTH_URL"      // external URL of the login issuer (auth.<domain>)
	VarServerImage  = "SHPYRD_SERVER_IMAGE"  // server image; derived from the version unless set
	// Local names and front door (RFC-0057).
	VarFrontDoor        = "SHPYRD_FRONT_DOOR"        // "kind" (kind maps the ports) or "caddy" (an existing Caddy on 443 proxies to kind)
	VarLocalDNS         = "SHPYRD_LOCAL_DNS"         // "true" when *.<domain> resolves through dnsmasq on this machine
	VarURLPort          = "SHPYRD_URL_PORT"          // derived: the https port public URLs carry ("443" behind Caddy)
	VarForwardedHeaders = "SHPYRD_FORWARDED_HEADERS" // derived: ingress-nginx trusts X-Forwarded-* only behind the front door
)

// Front door modes.
const (
	FrontDoorKind  = "kind"
	FrontDoorCaddy = "caddy"
)

// ServerImageRepo is where release workflows publish the server image.
const ServerImageRepo = "ghcr.io/shpyrd-io/shpyrd-server"

// releaseTagRe matches release tags (v1.2.3, v1.2.3-rc.1) but not what git
// describe makes of commits after a tag (v1.2.3-4-gabcdef, -dirty).
var releaseTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-(alpha|beta|rc)\.?\d*)?$`)

// DefaultServerImage is the image a CLI of the given version installs when
// nobody says otherwise (RFC-0045): a release installs the image of its own
// tag; a development build (git describe, "dev") gets the latest release,
// and developers point at their own build with --set SHPYRD_SERVER_IMAGE.
func DefaultServerImage(version string) string {
	if releaseTagRe.MatchString(version) {
		return ServerImageRepo + ":" + version
	}
	return ServerImageRepo + ":latest"
}

// derivedVars computes the variables manifests may use but nobody sets by
// hand: external URLs and the enabled extensions.
func derivedVars(vars map[string]string, exts []ExtensionComponent) map[string]string {
	base := BaseURL(vars)
	names := make([]string, 0, len(exts))
	for _, x := range exts {
		names = append(names, x.Extension)
	}
	out := map[string]string{
		VarDashboardURL:     base("shpyrd"),
		VarAuthURL:          base("auth"),
		VarExtensions:       strings.Join(names, ","),
		VarURLPort:          URLPort(vars),
		VarForwardedHeaders: "false",
	}
	if vars[VarFrontDoor] == FrontDoorCaddy {
		out[VarForwardedHeaders] = "true"
	}
	if vars[VarServerImage] == "" {
		out[VarServerImage] = DefaultServerImage(vars[VarVersion])
	}
	return out
}

// URLPort is the https port public URLs carry: 443 when a front door
// terminates TLS on the standard port, else the port kind maps.
func URLPort(vars map[string]string) string {
	if vars[VarFrontDoor] == FrontDoorCaddy {
		return "443"
	}
	if p := vars[VarHTTPSPort]; p != "" {
		return p
	}
	return "443"
}

// BaseURL returns a function building https URLs for <name>.<domain>,
// including the port when it is not 443.
func BaseURL(vars map[string]string) func(name string) string {
	domain := vars[VarDomain]
	port := URLPort(vars)
	return func(name string) string {
		u := "https://" + name + "." + domain
		if port != "443" {
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
