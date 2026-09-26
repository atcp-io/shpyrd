// Package backup exports the platform's state (RFC-0037): every shpyrd
// object, the config var Secrets and release snapshots, memberships, local
// accounts and the install record, plus the source archives builds need,
// as one tarball encrypted with age, and restores it on a fresh cluster.
// Data of databases and volumes has its own RFCs.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// FormatVersion of the archive layout.
const FormatVersion = 1

// Manifest describes an archive.
type Manifest struct {
	Version       int       `json:"version"`
	CreatedAt     time.Time `json:"createdAt"`
	Cluster       string    `json:"cluster"`
	Domain        string    `json:"domain,omitempty"`
	Profile       string    `json:"profile"`
	ShpyrdVersion string    `json:"shpyrdVersion"`
	Projects      []string  `json:"projects"`
	Objects       int       `json:"objects"`
	Sources       int       `json:"sources"`
}

// Kinds exported, in restore order. Namespaced ones are read from every
// project namespace; the cluster-scoped ones once.
var (
	projectKinds = []schema.GroupVersionResource{
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "apps"},
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "volumes"},
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "postgres"},
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "redis"},
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "logdrains"},
	}
	// Teams and grants live in the control-plane store since RFC-0033 and
	// travel as cluster/store.json; nothing cluster-scoped is left to list.
	clusterKinds = []schema.GroupVersionResource{}
	systemKinds  = []schema.GroupVersionResource{
		{Group: "dex.coreos.com", Version: "v1", Resource: "passwords"},
		{Group: "dex.coreos.com", Version: "v1", Resource: "connectors"},
	}
	// System objects worth carrying: the install record (for the operator
	// to read back what the cluster was: profile, domain, settings), the
	// size catalog and the global config vars. Not the admin token: the
	// server reads it at start, and the CLI reads whatever the new cluster
	// has.
	systemConfigMaps = []string{"shpyrd-install", "shpyrd-sizes"}
	systemSecrets    = []string{shpyrdv1.GlobalEnvSecretName}
)

// Exporter reads the cluster.
type Exporter struct {
	Kube            kubernetes.Interface
	Dynamic         dynamic.Interface
	SystemNamespace string
	// SourceBase is the server's in-cluster URL for source archives
	// (http://shpyrd-server.shpyrd-system.svc); empty skips sources.
	SourceBase string
	HTTP       *http.Client
	// Store is the control-plane database; nil skips it.
	Store store.Store
	// Domain, Cluster, Profile and Version describe the origin in the
	// manifest; the domain names the archives (cluster names default to
	// "shpyrd", domains are one per platform).
	Domain  string
	Cluster string
	Profile string
	Version string
}

// ArchiveName names an archive after the platform and the time.
func (e *Exporter) ArchiveName(at time.Time) string {
	base := e.Domain
	if base == "" {
		base = e.Cluster
	}
	if base == "" {
		base = "shpyrd"
	}
	return fmt.Sprintf("%s-%s.tar.gz.age", base, at.Format("20060102-150405"))
}

// Export writes the tar.gz archive (unencrypted) to w and returns its manifest.
func (e *Exporter) Export(ctx context.Context, w io.Writer) (*Manifest, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	man := &Manifest{Version: FormatVersion, CreatedAt: time.Now().UTC(), Cluster: e.Cluster, Domain: e.Domain, Profile: e.Profile, ShpyrdVersion: e.Version}
	add := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: man.CreatedAt, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	addObjects := func(name string, items []unstructured.Unstructured) error {
		if len(items) == 0 {
			return nil
		}
		var buf bytes.Buffer
		for _, it := range items {
			clean(&it)
			b, err := yaml.Marshal(it.Object)
			if err != nil {
				return err
			}
			buf.WriteString("---\n")
			buf.Write(b)
		}
		man.Objects += len(items)
		return add(name, buf.Bytes())
	}

	// System: the install record and friends.
	for _, name := range systemConfigMaps {
		cm, err := e.Kube.CoreV1().ConfigMaps(e.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		u, _ := toUnstructured(cm, "v1", "ConfigMap")
		if err := addObjects("system/configmap-"+name+".yaml", []unstructured.Unstructured{*u}); err != nil {
			return nil, err
		}
	}
	for _, name := range systemSecrets {
		sec, err := e.Kube.CoreV1().Secrets(e.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		u, _ := toUnstructured(sec, "v1", "Secret")
		if err := addObjects("system/secret-"+name+".yaml", []unstructured.Unstructured{*u}); err != nil {
			return nil, err
		}
	}
	for _, gvr := range systemKinds {
		list, err := e.Dynamic.Resource(gvr).Namespace(e.SystemNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue // extension not installed
		}
		if err := addObjects("system/"+gvr.Resource+".yaml", list.Items); err != nil {
			return nil, err
		}
	}
	for _, gvr := range clusterKinds {
		list, err := e.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		if err := addObjects("cluster/"+gvr.Resource+".yaml", list.Items); err != nil {
			return nil, err
		}
	}
	// The control-plane store: the workspace, its people, teams and grants.
	if e.Store != nil {
		dump, err := e.Store.Export(ctx, store.DefaultWorkspace)
		if err != nil {
			return nil, fmt.Errorf("export store: %w", err)
		}
		raw, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := add("cluster/store.json", raw); err != nil {
			return nil, err
		}
		man.Objects += len(dump.Teams) + len(dump.Grants)
	}

	// Projects: every namespace carrying the project label.
	nsList, err := e.Kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: shpyrdv1.LabelProject})
	if err != nil {
		return nil, fmt.Errorf("list project namespaces: %w", err)
	}
	sources := map[string]bool{}
	for _, ns := range nsList.Items {
		man.Projects = append(man.Projects, ns.Labels[shpyrdv1.LabelProject])
		nsCopy := ns
		nsCopy.Spec = corev1.NamespaceSpec{}
		u, _ := toUnstructured(&nsCopy, "v1", "Namespace")
		if err := addObjects("projects/"+ns.Name+"/namespace.yaml", []unstructured.Unstructured{*u}); err != nil {
			return nil, err
		}
		for _, gvr := range projectKinds {
			list, err := e.Dynamic.Resource(gvr).Namespace(ns.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			if gvr.Resource == "apps" {
				for _, app := range list.Items {
					if u, ok, _ := unstructured.NestedString(app.Object, "spec", "source", "blob", "sha256"); ok && u != "" {
						sources[u] = true
					}
				}
			}
			if err := addObjects("projects/"+ns.Name+"/"+gvr.Resource+".yaml", list.Items); err != nil {
				return nil, err
			}
		}
		// The platform's Secrets: config vars (shpyrd.io/app) and what
		// else it manages. Not the ones controllers regenerate (registry
		// credential, bucket keys, TLS), not other operators' (CNPG's
		// own), and not release snapshots: the release history does not
		// travel (a restored app starts at v1).
		secrets, err := e.Kube.CoreV1().Secrets(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		var keep []unstructured.Unstructured
		for _, sec := range secrets.Items {
			_, ofApp := sec.Labels[shpyrdv1.LabelApp]
			if !ofApp && sec.Labels[shpyrdv1.LabelManagedBy] != "shpyrd" {
				continue
			}
			if _, snapshot := sec.Labels[shpyrdv1.LabelRelease]; snapshot || !backupWorthy(sec.Name) {
				continue
			}
			u, _ := toUnstructured(&sec, "v1", "Secret")
			keep = append(keep, *u)
		}
		if err := addObjects("projects/"+ns.Name+"/secrets.yaml", keep); err != nil {
			return nil, err
		}
	}
	sort.Strings(man.Projects)

	// Source archives builds run from: content-addressed, fetched from the
	// server the way kpack fetches them.
	if e.SourceBase != "" {
		httpc := e.HTTP
		if httpc == nil {
			httpc = &http.Client{Timeout: 2 * time.Minute}
		}
		for sha := range sources {
			req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(e.SourceBase, "/")+"/api/sources/"+sha+".tgz", nil)
			resp, err := httpc.Do(req)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", sha, err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("source %s: %s", sha, resp.Status)
			}
			if err := add("sources/"+sha+".tgz", data); err != nil {
				return nil, err
			}
			man.Sources++
		}
	}

	mb, _ := json.MarshalIndent(man, "", "  ")
	if err := add("manifest.json", mb); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return man, gz.Close()
}

// backupWorthy says whether a managed Secret in a project namespace is
// state (config vars, release snapshots, bound vars) rather than something
// a controller regenerates on the new cluster.
func backupWorthy(name string) bool {
	switch {
	case name == "shpyrd-registry", strings.HasSuffix(name, "-object-storage"), strings.HasSuffix(name, "-tls"):
		return false
	}
	return true
}

// clean strips what does not survive a move between clusters.
func clean(u *unstructured.Unstructured) {
	unstructured.RemoveNestedField(u.Object, "status")
	meta := u.Object["metadata"].(map[string]interface{})
	for _, k := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "ownerReferences", "selfLink", "finalizers", "deletionTimestamp", "deletionGracePeriodSeconds"} {
		delete(meta, k)
	}
	if ann, ok := meta["annotations"].(map[string]interface{}); ok {
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		if len(ann) == 0 {
			delete(meta, "annotations")
		}
	}
}

func toUnstructured(obj interface{}, apiVersion, kind string) (*unstructured.Unstructured, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(b, &u.Object); err != nil {
		return nil, err
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	return u, nil
}

// Encrypt wraps the archive with age (scrypt passphrase).
func Encrypt(w io.Writer, passphrase string) (io.WriteCloser, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	return age.Encrypt(w, r)
}

// Decrypt opens an age-encrypted archive.
func Decrypt(r io.Reader, passphrase string) (io.Reader, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	out, err := age.Decrypt(r, id)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w (wrong passphrase?)", err)
	}
	return out, nil
}

// Archive is a decoded backup.
type Archive struct {
	Manifest Manifest
	Files    map[string][]byte // path -> content
}

// Read parses a decrypted tar.gz archive.
func Read(r io.Reader) (*Archive, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	tr := tar.NewReader(gz)
	a := &Archive{Files: map[string][]byte{}}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		a.Files[hdr.Name] = data
	}
	if m, ok := a.Files["manifest.json"]; ok {
		if err := json.Unmarshal(m, &a.Manifest); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
	} else {
		return nil, fmt.Errorf("not a shpyrd backup (no manifest)")
	}
	return a, nil
}

// Objects decodes the YAML documents of a file.
func (a *Archive) Objects(path string) ([]unstructured.Unstructured, error) {
	data, ok := a.Files[path]
	if !ok {
		return nil, nil
	}
	var out []unstructured.Unstructured
	for _, doc := range strings.Split(string(data), "\n---\n") {
		doc = strings.TrimSpace(strings.TrimPrefix(doc, "---"))
		if doc == "" {
			continue
		}
		u := unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if u.GetKind() != "" {
			out = append(out, u)
		}
	}
	return out, nil
}

// Paths lists files with a prefix, sorted.
func (a *Archive) Paths(prefix string) []string {
	var out []string
	for p := range a.Files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
