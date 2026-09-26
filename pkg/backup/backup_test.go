package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/shpyrd-io/shpyrd/pkg/store"
)

// RFC-0037: the export carries the install record, cluster objects, every
// project's objects and state Secrets, and the source archives; the age
// round trip needs the passphrase.
func TestExportEncryptReadRoundTrip(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sources/abc123.tgz" {
			_, _ = w.Write([]byte("tarball"))
			return
		}
		http.NotFound(w, r)
	}))
	defer src.Close()

	kube := kubefake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-shop", Labels: map[string]string{"shpyrd.io/project": "shop"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shpyrd-install", Namespace: "shpyrd-system"}, Data: map[string]string{"profile": "oci"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shpyrd-global-env", Namespace: "shpyrd-system"}, Data: map[string][]byte{"TZ": []byte("UTC")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shop-env", Namespace: "app-shop", Labels: map[string]string{"shpyrd.io/app": "shop"}, UID: "u1", ResourceVersion: "9"}, Data: map[string][]byte{"GREETING": []byte("hi")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shop-release-v1", Namespace: "app-shop", Labels: map[string]string{"app.kubernetes.io/managed-by": "shpyrd", "shpyrd.io/app": "shop", "shpyrd.io/release": "1"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-app", Namespace: "app-shop", Labels: map[string]string{"app.kubernetes.io/managed-by": "cloudnative-pg"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shpyrd-registry", Namespace: "app-shop", Labels: map[string]string{"app.kubernetes.io/managed-by": "shpyrd"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-backups-object-storage", Namespace: "app-shop", Labels: map[string]string{"app.kubernetes.io/managed-by": "shpyrd"}}},
	)
	scheme := runtime.NewScheme()
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "shpyrd.io/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "shop", "namespace": "app-shop", "uid": "x", "resourceVersion": "3", "ownerReferences": []interface{}{map[string]interface{}{"kind": "X"}}},
		"spec":     map[string]interface{}{"source": map[string]interface{}{"blob": map[string]interface{}{"sha256": "abc123", "url": "http://shpyrd-server.shpyrd-system.svc/api/sources/abc123.tgz"}}},
		"status":   map[string]interface{}{"phase": "Running"},
	}}
	team := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "shpyrd.io/v1alpha1", "kind": "Team",
		"metadata": map[string]interface{}{"name": "platform"},
		"spec":     map[string]interface{}{"members": []interface{}{"ops@example.test"}},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "apps"}:           "AppList",
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "volumes"}:        "VolumeList",
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "postgres"}:       "PostgresList",
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "redis"}:          "RedisList",
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "logdrains"}:      "LogDrainList",
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "teams"}:          "TeamList",
		{Group: "shpyrd.io", Version: "v1alpha1", Resource: "projectmembers"}: "ProjectMemberList",
		{Group: "dex.coreos.com", Version: "v1", Resource: "passwords"}:       "PasswordList",
		{Group: "dex.coreos.com", Version: "v1", Resource: "connectors"}:      "ConnectorList",
	}, app, team)

	st := store.NewMemory()
	if _, _, err := st.PutTeam(context.Background(), store.DefaultWorkspace, store.Team{Name: "platform", Members: []string{"ops@example.test"}}); err != nil {
		t.Fatal(err)
	}
	e := &Exporter{Kube: kube, Dynamic: dyn, SystemNamespace: "shpyrd-system", SourceBase: src.URL, Cluster: "dev", Profile: "oci", Version: "v0.3.4", Store: st}
	var plain bytes.Buffer
	man, err := e.Export(context.Background(), &plain)
	if err != nil {
		t.Fatal(err)
	}
	if man.Sources != 1 || len(man.Projects) != 1 || man.Projects[0] != "shop" || man.Objects < 5 {
		t.Errorf("manifest = %+v", man)
	}

	// Encrypt, fail with the wrong passphrase, succeed with the right one.
	var enc bytes.Buffer
	w, err := Encrypt(&enc, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(w, bytes.NewReader(plain.Bytes()))
	_ = w.Close()
	if _, err := Decrypt(bytes.NewReader(enc.Bytes()), "wrong"); err == nil {
		t.Error("wrong passphrase must fail")
	}
	dec, err := Decrypt(bytes.NewReader(enc.Bytes()), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	a, err := Read(dec)
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest.Cluster != "dev" || string(a.Files["sources/abc123.tgz"]) != "tarball" {
		t.Errorf("archive = %+v", a.Manifest)
	}
	apps, err := a.Objects("projects/app-shop/apps.yaml")
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps: %v %d", err, len(apps))
	}
	if _, has := apps[0].Object["status"]; has {
		t.Error("status must be stripped")
	}
	if apps[0].GetUID() != "" || apps[0].GetResourceVersion() != "" || len(apps[0].GetOwnerReferences()) != 0 {
		t.Error("identity fields must be stripped")
	}
	secrets, _ := a.Objects("projects/app-shop/secrets.yaml")
	names := map[string]bool{}
	for _, s := range secrets {
		names[s.GetName()] = true
	}
	if !names["shop-env"] || names["shop-release-v1"] || names["db-app"] || names["shpyrd-registry"] || names["db-backups-object-storage"] {
		t.Errorf("secrets kept = %v", names)
	}
	var dump store.Dump
	if err := json.Unmarshal(a.Files["cluster/store.json"], &dump); err != nil || len(dump.Teams) != 2 || dump.Teams[1].Name != "platform" {
		t.Errorf("store dump: %v %+v", err, dump)
	}
	if _, has := a.Files["system/configmap-shpyrd-install.yaml"]; !has {
		t.Error("install record missing")
	}
	if len(a.Paths("projects/app-shop/")) < 3 {
		t.Errorf("project files = %v", a.Paths("projects/app-shop/"))
	}
}

func TestParseURI(t *testing.T) {
	b, p, err := ParseURI("s3://my-bucket/platform/dev")
	if err != nil || b != "my-bucket" || p != "platform/dev" {
		t.Errorf("%s %s %v", b, p, err)
	}
	if _, _, err := ParseURI("https://x"); err == nil {
		t.Error("scheme must be s3")
	}
}

// Restore into an empty cluster creates the project's objects in
// dependency order and uploads the sources; a second run skips the project.
func TestRestore(t *testing.T) {
	a := &Archive{Manifest: Manifest{Projects: []string{"shop"}}, Files: map[string][]byte{
		"system/configmap-shpyrd-install.yaml": []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: shpyrd-install, namespace: shpyrd-system}\ndata:\n  profile: oci\n  vars: |\n    SHPYRD_DOMAIN: oci.example.test\n"),
		"system/secret-shpyrd-global-env.yaml": []byte("---\napiVersion: v1\nkind: Secret\nmetadata: {name: shpyrd-global-env, namespace: shpyrd-system}\ndata: {TZ: VVRD}\n"),
		"cluster/teams.yaml":                   []byte("---\napiVersion: shpyrd.io/v1alpha1\nkind: Team\nmetadata: {name: platform}\nspec: {members: [ops@example.test]}\n"),
		"projects/app-shop/namespace.yaml":     []byte("---\napiVersion: v1\nkind: Namespace\nmetadata: {name: app-shop, labels: {shpyrd.io/project: shop}}\n"),
		"projects/app-shop/secrets.yaml":       []byte("---\napiVersion: v1\nkind: Secret\nmetadata: {name: shop-env, namespace: app-shop}\ndata: {A: Yg==}\n"),
		"projects/app-shop/postgres.yaml":      []byte("---\napiVersion: shpyrd.io/v1alpha1\nkind: Postgres\nmetadata: {name: db, namespace: app-shop}\nspec: {size: small}\n"),
		"projects/app-shop/apps.yaml":          []byte("---\napiVersion: shpyrd.io/v1alpha1\nkind: App\nmetadata: {name: shop, namespace: app-shop}\nspec: {source: {blob: {sha256: abc123}}}\n"),
		"sources/abc123.tgz":                   []byte("tarball"),
	}}
	profile, vars := a.InstallRecord()
	if profile != "oci" || vars["SHPYRD_DOMAIN"] != "oci.example.test" {
		t.Errorf("install record: %s %v", profile, vars)
	}
	if got := a.ProjectNamespaces(); len(got) != 1 || got[0] != "app-shop" {
		t.Errorf("namespaces = %v", got)
	}

	dyn := dynfake.NewSimpleDynamicClient(runtime.NewScheme())
	var order []string
	uploaded := map[string]int{}
	imported := &store.Dump{}
	r := &Restorer{Dynamic: dyn, Archive: a, System: true,
		UploadSource: func(_ context.Context, sha string, data []byte) error { uploaded[sha] += len(data); return nil },
		ImportStore:  func(_ context.Context, d *store.Dump, _ bool) error { imported = d; return nil },
		Log:          func(f string, args ...any) { order = append(order, fmt.Sprintf(f, args...)) }}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 6 || res.Sources != 1 || uploaded["abc123"] != 7 || len(res.Projects) != 1 {
		t.Errorf("result = %+v uploaded=%v", res, uploaded)
	}
	// An archive from before the store carries Team objects: they become a dump.
	if len(imported.Teams) != 1 || imported.Teams[0].Name != "platform" || len(imported.Teams[0].Members) != 1 {
		t.Errorf("imported = %+v", imported)
	}
	joined := strings.Join(order, "\n")
	if strings.Index(joined, "postgres app-shop/db") > strings.Index(joined, "app app-shop/shop") {
		t.Errorf("postgres must come before the app:\n%s", joined)
	}
	if strings.Contains(joined, "shpyrd-install") {
		t.Error("the install record must not be applied")
	}
	// The project namespace exists now: the whole project is skipped.
	res2, err := (&Restorer{Dynamic: dyn, Archive: a}).Run(context.Background())
	if err != nil || len(res2.SkippedProjects) != 1 || res2.Created != 0 {
		t.Errorf("second run = %+v %v", res2, err)
	}
	// --overwrite updates in place.
	res3, err := (&Restorer{Dynamic: dyn, Archive: a, Overwrite: true}).Run(context.Background())
	if err != nil || res3.Updated != 3 {
		t.Errorf("overwrite run = %+v %v", res3, err)
	}
	// An unknown project is an error naming what the archive has.
	if _, err := (&Restorer{Dynamic: dyn, Archive: a, Projects: []string{"nope"}}).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "shop") {
		t.Errorf("unknown project: %v", err)
	}
}
