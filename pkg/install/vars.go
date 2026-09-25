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
	// Cloud profiles (RFC-0034/0035 counterparts).
	VarClusterIssuer    = "SHPYRD_CLUSTER_ISSUER"    // cert-manager ClusterIssuer for every certificate (shpyrd-ca locally, letsencrypt on cloud)
	VarACMEEmail        = "SHPYRD_ACME_EMAIL"        // Let's Encrypt account email (cloud profiles)
	VarRegistryInsecure = "SHPYRD_REGISTRY_INSECURE" // "true" keeps the in-cluster registry on plain HTTP (escape hatch, RFC-0059)
	VarRegistrySecret   = "SHPYRD_REGISTRY_SECRET"   // name of the registry credentials Secret ("" when the registry needs none)
	// In-cluster registry (RFC-0059).
	VarRegistryIP   = "SHPYRD_REGISTRY_IP"   // fixed ClusterIP of the in-cluster registry ("" with an external registry)
	VarRegistrySize = "SHPYRD_REGISTRY_SIZE" // size of its volume claim
	VarCASource     = "SHPYRD_CA_SOURCE"     // where the platform CA comes from: "local" (~/.shpyrd/ca, shared by kind clusters) or "cluster" (generated once in the cluster)
	// Network policy enforcement (RFC-0035): "calico" installs Calico in
	// policy-only mode next to the provider's CNI; "none" relies on the
	// cluster's own engine (kind's kindnet enforces policies).
	VarNetworkPolicy = "SHPYRD_NETWORK_POLICY"
	// DNS provider (RFC-0061): "none" or "oci". The credential travels in
	// Secrets written by the dns-credentials hook, never in variables.
	VarDNSProvider    = "SHPYRD_DNS_PROVIDER"
	VarDNSAuth        = "SHPYRD_DNS_AUTH"        // "key" (an API signing key) or "workload" (OKE workload identity)
	VarDNSCompartment = "SHPYRD_DNS_COMPARTMENT" // compartment holding the zone
	VarDNSTenancy     = "SHPYRD_DNS_TENANCY"
	VarDNSRegion      = "SHPYRD_DNS_REGION"
	VarDNSUser        = "SHPYRD_DNS_USER"    // IAM user of the API key
	VarDNSZoneID      = "SHPYRD_DNS_ZONE_ID" // Route 53 hosted zone id (aws)
	// Derived from the DNS settings (see derivedVars).
	VarDNSProfileSecret  = "SHPYRD_DNS_PROFILE_SECRET"  // the webhook's credential Secret name ("" with workload identity)
	VarDNSProfileSecrets = "SHPYRD_DNS_PROFILE_SECRETS" // the same as a YAML list body
	VarDefaultTLSSecret  = "SHPYRD_DEFAULT_TLS_SECRET"  // namespace/name of ingress-nginx's default certificate
	VarWildcardTLS       = "SHPYRD_WILDCARD_TLS"        // "true" when the wildcard certificate serves every project host
	// Front doors (RFC-0036).
	VarPlatformExposure     = "SHPYRD_PLATFORM_EXPOSURE"      // "external" or "internal"
	VarInternalLB           = "SHPYRD_INTERNAL_LB"            // "auto" (create lazily), "true", "false"
	VarInternalLBSubnet     = "SHPYRD_INTERNAL_LB_SUBNET"     // subnet OCID for the private LB (OCI)
	VarIngressClassInternal = "SHPYRD_INGRESS_CLASS_INTERNAL" // ingress class of the internal controller
	VarIngressClassExternal = "SHPYRD_INGRESS_CLASS_EXTERNAL" // ingress class of the external controller
	// Volumes on cloud profiles (RFC-0060).
	VarStorageClass       = "SHPYRD_STORAGE_CLASS"        // class for single-instance volumes ("" = the cluster default)
	VarStorageClassShared = "SHPYRD_STORAGE_CLASS_SHARED" // class for shared (ReadWriteMany) volumes
	VarVolumeMinSize      = "SHPYRD_VOLUME_MIN_SIZE"      // provider minimum a request is rounded up to ("" = none)
	VarSnapshotClass      = "SHPYRD_SNAPSHOT_CLASS"       // VolumeSnapshotClass for `shpyrd volumes snapshot` ("" = snapshots unavailable)
	VarFSSMountTarget     = "SHPYRD_FSS_MOUNT_TARGET"     // OCI File Storage mount target OCID behind shared volumes ("" = no shared volumes)
	VarFSSAD              = "SHPYRD_FSS_AD"               // availability domain of the shared volumes' file systems (OCI)
	VarEFSID              = "SHPYRD_EFS_ID"               // EFS file system behind shared volumes ("" = no shared volumes) (AWS)
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
	// FrontDoorLB is a cloud load balancer in front of ingress-nginx.
	FrontDoorLB = "lb"
)

// RegistrySecretName is the dockerconfigjson Secret with the credentials
// builds push with and instances pull with (private registries).
const RegistrySecretName = "shpyrd-registry"

// RegistryHtpasswdSecretName holds the htpasswd file the in-cluster registry
// authenticates against (key "htpasswd").
const RegistryHtpasswdSecretName = "registry-htpasswd"

// DNS automation (RFC-0061).
const (
	DNSAuthKey      = "key"
	DNSAuthWorkload = "workload"
	// DNSConfigSecretName holds ExternalDNS's oci.yaml (system namespace).
	DNSConfigSecretName = "external-dns-config"
	// DNSProfileSecretName holds the API key for the DNS-01 webhook
	// (cert-manager namespace).
	DNSProfileSecretName = "oci-dns"
	// WildcardTLSSecretName is the platform's wildcard certificate.
	WildcardTLSSecretName = "platform-wildcard-tls"
)

// Platform CA sources (SHPYRD_CA_SOURCE).
const (
	CASourceLocal   = "local"
	CASourceCluster = "cluster"
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
	// DNS (RFC-0061): with a provider, one wildcard certificate is the front
	// door's default and project Ingresses carry none of their own.
	out[VarDNSProfileSecret], out[VarDNSProfileSecrets] = "", ""
	if vars[VarDNSProvider] != "" && vars[VarDNSProvider] != "none" && vars[VarDNSAuth] != DNSAuthWorkload {
		out[VarDNSProfileSecret], out[VarDNSProfileSecrets] = DNSProfileSecretName, DNSProfileSecretName
	}
	out[VarDefaultTLSSecret] = DefaultSystemNamespace + "/shpyrd-tls"
	out[VarWildcardTLS] = "false"
	if vars[VarDNSProvider] != "" && vars[VarDNSProvider] != "none" {
		out[VarDefaultTLSSecret] = DefaultSystemNamespace + "/" + WildcardTLSSecretName
		out[VarWildcardTLS] = "true"
	}
	return out
}

// URLPort is the https port public URLs carry: 443 when a front door
// terminates TLS on the standard port or a cloud load balancer listens
// there, else the port kind maps.
func URLPort(vars map[string]string) string {
	if vars[VarFrontDoor] == FrontDoorCaddy || vars[VarFrontDoor] == FrontDoorLB {
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
