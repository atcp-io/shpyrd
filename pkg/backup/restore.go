package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/store"
)

// Restorer puts an archive back into a cluster that already runs the
// platform (`shpyrd cluster init` first: CRDs, server, extensions). System
// objects (sizes, globals, sign-in users, teams, members) are created or
// replaced; a project is created when its namespace is absent and skipped
// otherwise, unless Overwrite says to update its objects in place.
type Restorer struct {
	Dynamic dynamic.Interface
	Archive *Archive
	// Projects limits the restore to these slugs (nil: all).
	Projects []string
	// System restores the system and cluster objects too.
	System    bool
	Overwrite bool
	// UploadSource stores a source archive on the server (POST
	// /api/sources); nil skips sources.
	UploadSource func(ctx context.Context, sha string, data []byte) error
	// ImportStore puts the control-plane dump back (POST
	// /api/workspace/import); nil skips teams and grants.
	ImportStore func(ctx context.Context, dump *store.Dump, overwrite bool) error
	// Log receives one line per step.
	Log func(format string, args ...any)
}

// Result counts what happened.
type Result struct {
	Created, Updated, Skipped int
	Sources                   int
	Projects                  []string // restored
	SkippedProjects           []string // namespace existed
	Warnings                  []string
}

// restoreOrder puts what apps depend on first.
var restoreOrder = []string{"volumes", "postgres", "redis", "logdrains", "apps"}

// Run applies the archive.
func (r *Restorer) Run(ctx context.Context) (*Result, error) {
	res := &Result{}
	if r.Log == nil {
		r.Log = func(string, ...any) {}
	}
	if r.System {
		if err := r.restoreSystem(ctx, res); err != nil {
			return res, err
		}
	}
	want := map[string]bool{}
	for _, p := range r.Projects {
		want[p] = true
	}
	for _, ns := range r.Archive.ProjectNamespaces() {
		slug := strings.TrimPrefix(ns, "app-")
		if len(want) > 0 && !want[slug] {
			continue
		}
		if err := r.restoreProject(ctx, ns, slug, res); err != nil {
			return res, fmt.Errorf("project %s: %w", slug, err)
		}
	}
	if len(want) > 0 {
		var missing []string
		for slug := range want {
			found := false
			for _, ns := range r.Archive.ProjectNamespaces() {
				if strings.TrimPrefix(ns, "app-") == slug {
					found = true
				}
			}
			if !found {
				missing = append(missing, slug)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			return res, fmt.Errorf("not in the archive: %s (it has: %s)", strings.Join(missing, ", "), strings.Join(r.Archive.Manifest.Projects, ", "))
		}
	}
	return res, nil
}

// ProjectNamespaces lists the project namespaces in the archive, sorted.
func (a *Archive) ProjectNamespaces() []string {
	seen := map[string]bool{}
	for _, p := range a.Paths("projects/") {
		parts := strings.Split(p, "/")
		if len(parts) >= 3 {
			seen[parts[1]] = true
		}
	}
	var out []string
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func (r *Restorer) restoreSystem(ctx context.Context, res *Result) error {
	// The install record is the new cluster's business: `cluster init`
	// wrote what this infrastructure has. It travels in the archive for
	// reading, not applying.
	for _, path := range r.Archive.Paths("system/") {
		if path == "system/configmap-shpyrd-install.yaml" {
			continue
		}
		if err := r.applyFile(ctx, path, true, res); err != nil {
			return err
		}
	}
	for _, path := range r.Archive.Paths("cluster/") {
		if path == "cluster/store.json" || path == "cluster/teams.yaml" || path == "cluster/projectmembers.yaml" {
			continue // the store, below
		}
		if err := r.applyFile(ctx, path, true, res); err != nil {
			return err
		}
	}
	return r.restoreStore(ctx, res)
}

// restoreStore imports teams and grants: from cluster/store.json, or, for
// archives made before the control-plane store, from the Team and
// ProjectMember objects they carry.
func (r *Restorer) restoreStore(ctx context.Context, res *Result) error {
	if r.ImportStore == nil {
		return nil
	}
	var dump store.Dump
	if raw, ok := r.Archive.Files["cluster/store.json"]; ok {
		if err := json.Unmarshal(raw, &dump); err != nil {
			return fmt.Errorf("cluster/store.json: %w", err)
		}
	} else {
		teams, _ := r.Archive.Objects("cluster/teams.yaml")
		for _, u := range teams {
			members, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "members")
			groups, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "groups")
			desc, _, _ := unstructured.NestedString(u.Object, "spec", "description")
			role, _, _ := unstructured.NestedString(u.Object, "spec", "platformRole")
			dump.Teams = append(dump.Teams, store.Team{Name: u.GetName(), Description: desc, Members: members, Groups: groups, PlatformRole: role})
		}
		members, _ := r.Archive.Objects("cluster/projectmembers.yaml")
		for _, u := range members {
			project, _, _ := unstructured.NestedString(u.Object, "spec", "project")
			role, _, _ := unstructured.NestedString(u.Object, "spec", "role")
			user, _, _ := unstructured.NestedString(u.Object, "spec", "user")
			team, _, _ := unstructured.NestedString(u.Object, "spec", "team")
			dump.Grants = append(dump.Grants, store.Grant{Project: project, Role: role, User: user, Team: team})
		}
		dump.Version = store.DumpVersion
	}
	if len(dump.Teams) == 0 && len(dump.Grants) == 0 && len(dump.Identities) == 0 {
		return nil
	}
	if err := r.ImportStore(ctx, &dump, r.Overwrite); err != nil {
		return fmt.Errorf("teams and grants: %w", err)
	}
	res.Created += len(dump.Teams) + len(dump.Grants)
	r.Log("  teams %d, grants %d, people %d restored", len(dump.Teams), len(dump.Grants), len(dump.Identities))
	return nil
}

func (r *Restorer) restoreProject(ctx context.Context, ns, slug string, res *Result) error {
	nsObjs, err := r.Archive.Objects("projects/" + ns + "/namespace.yaml")
	if err != nil || len(nsObjs) != 1 {
		return fmt.Errorf("namespace.yaml missing")
	}
	_, err = r.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, ns, metav1.GetOptions{})
	switch {
	case err == nil && !r.Overwrite:
		r.Log("project %s: namespace %s exists, skipped (delete the project first, or --overwrite to update its objects)", slug, ns)
		res.SkippedProjects = append(res.SkippedProjects, slug)
		return nil
	case err == nil:
		r.Log("project %s: namespace %s exists, updating its objects", slug, ns)
	case apierrors.IsNotFound(err):
		if _, err := r.apply(ctx, &nsObjs[0], false); err != nil {
			return err
		}
		res.Created++
		r.Log("project %s: namespace %s created", slug, ns)
	default:
		return err
	}
	res.Projects = append(res.Projects, slug)

	if err := r.applyFile(ctx, "projects/"+ns+"/secrets.yaml", r.Overwrite, res); err != nil {
		return err
	}
	// Sources before the apps that build from them.
	if apps, err := r.Archive.Objects("projects/" + ns + "/apps.yaml"); err == nil {
		for _, app := range apps {
			sha, _, _ := unstructured.NestedString(app.Object, "spec", "source", "blob", "sha256")
			if sha == "" {
				continue
			}
			data, ok := r.Archive.Files["sources/"+sha+".tgz"]
			if !ok {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: source %s is not in the archive; deploy it again", app.GetName(), sha[:12]))
				continue
			}
			if r.UploadSource == nil {
				continue
			}
			if err := r.UploadSource(ctx, sha, data); err != nil {
				return fmt.Errorf("upload source %s: %w", sha[:12], err)
			}
			res.Sources++
		}
	}
	for _, kind := range restoreOrder {
		if err := r.applyFile(ctx, "projects/"+ns+"/"+kind+".yaml", r.Overwrite, res); err != nil {
			return err
		}
	}
	return nil
}

// applyFile creates every object in a file; update says what to do with
// existing ones (replace or count as skipped). A missing file is fine.
func (r *Restorer) applyFile(ctx context.Context, path string, update bool, res *Result) error {
	if _, ok := r.Archive.Files[path]; !ok {
		return nil
	}
	objs, err := r.Archive.Objects(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for i := range objs {
		obj := &objs[i]
		outcome, err := r.apply(ctx, obj, update)
		if err != nil {
			if apierrors.IsNotFound(err) && strings.Contains(err.Error(), "the server could not find the requested resource") {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s %s: kind not served by this cluster (extension not enabled?), skipped", obj.GetKind(), obj.GetName()))
				continue
			}
			return fmt.Errorf("%s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		switch outcome {
		case "created":
			res.Created++
		case "updated":
			res.Updated++
		default:
			res.Skipped++
		}
		r.Log("  %s %s/%s %s", strings.ToLower(obj.GetKind()), obj.GetNamespace(), obj.GetName(), outcome)
	}
	return nil
}

// apply creates the object, or updates it when it exists and update is set.
func (r *Restorer) apply(ctx context.Context, obj *unstructured.Unstructured, update bool) (string, error) {
	gvr, err := gvrFor(obj)
	if err != nil {
		return "", err
	}
	var ri dynamic.ResourceInterface = r.Dynamic.Resource(gvr)
	if obj.GetNamespace() != "" {
		ri = r.Dynamic.Resource(gvr).Namespace(obj.GetNamespace())
	}
	_, err = ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: "shpyrd-restore"})
	if err == nil {
		return "created", nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	if !update {
		return "exists", nil
	}
	existing, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	obj.SetUID(existing.GetUID())
	if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: "shpyrd-restore"}); err != nil {
		return "", err
	}
	return "updated", nil
}

// gvrFor maps the kinds an archive holds to their resources.
func gvrFor(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	resource, ok := map[string]string{
		"Namespace": "namespaces", "ConfigMap": "configmaps", "Secret": "secrets",
		"App": "apps", "Volume": "volumes", "Postgres": "postgres", "Redis": "redis", "LogDrain": "logdrains",
		"Team": "teams", "ProjectMember": "projectmembers",
		"Password": "passwords", "Connector": "connectors",
	}[obj.GetKind()]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("kind %s is not restorable", obj.GetKind())
	}
	return gv.WithResource(resource), nil
}

// InstallRecord reads the archived install record (profile, vars) for the
// operator to compare with the target cluster.
func (a *Archive) InstallRecord() (profile string, vars map[string]string) {
	objs, err := a.Objects("system/configmap-shpyrd-install.yaml")
	if err != nil || len(objs) != 1 {
		return "", nil
	}
	data, _, _ := unstructured.NestedStringMap(objs[0].Object, "data")
	vars = map[string]string{}
	for _, line := range strings.Split(data["vars"], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return data["profile"], vars
}

var _ = shpyrdv1.LabelProject
