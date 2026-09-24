package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"
	"shpyrd/pkg/install"
)

// RFC-0060: the provider minimum rounds a request up with a note, snapshots
// are refused where the profile has no class, and with one they can be
// taken, listed, restored (new volume or in place) and deleted.
func TestVolumeMinimumAndSnapshots(t *testing.T) {
	app := &shpyrdv1.App{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "app-demo"}}
	s, cr := newTestServer(t, nil, []client.Object{app})

	// No class: snapshots say so; sizes pass through untouched.
	rec := do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"data","size":"1Gi"}`, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"size":"1Gi"`) || strings.Contains(rec.Body.String(), `"note"`) {
		t.Fatalf("create without minimum: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/projects/demo/volumes/data/snapshots", "", true); rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "not available") {
		t.Errorf("snapshots without class: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/demo/volumes/data/restore", `{"snapshot":"x"}`, true); rec.Code != http.StatusNotImplemented {
		t.Errorf("restore without class: %d", rec.Code)
	}

	// The oci profile: 50Gi minimum, a snapshot class.
	vars := map[string]string{install.VarVolumeMinSize: "50Gi", install.VarSnapshotClass: "oci-bv-backup", install.VarProfile: "oci"}
	s.opts.Vars = func(name string) string { return vars[name] }

	rec = do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"small","size":"1Gi"}`, true)
	var view VolumeView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != http.StatusCreated || view.Size != "50Gi" || !strings.Contains(view.Note, "start at 50Gi") || !strings.Contains(view.Note, "instead of 1Gi") {
		t.Errorf("rounded create: %d %+v", rec.Code, view)
	}
	rec = do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"big","size":"100Gi"}`, true)
	view = VolumeView{}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != http.StatusCreated || view.Size != "100Gi" || view.Note != "" {
		t.Errorf("above minimum: %d %+v", rec.Code, view)
	}
	if rec := do(t, s, "GET", "/api/config", "", false); !strings.Contains(rec.Body.String(), `"minSize":"50Gi"`) || !strings.Contains(rec.Body.String(), `"snapshots":true`) {
		t.Errorf("config: %s", rec.Body.String())
	}
	// Shared volumes without the shared class installed: refused up front.
	vars[install.VarStorageClassShared] = "shpyrd-fss"
	if rec := do(t, s, "POST", "/api/projects/demo/volumes", `{"name":"media","size":"1Gi","shared":true}`, true); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "SHPYRD_FSS_MOUNT_TARGET") {
		t.Errorf("shared without class: %d %s", rec.Code, rec.Body.String())
	}

	// A snapshot needs a disk.
	if rec := do(t, s, "POST", "/api/projects/demo/volumes/data/snapshots", `{}`, true); rec.Code != http.StatusConflict {
		t.Errorf("snapshot of unbound volume: %d %s", rec.Code, rec.Body.String())
	}
	vol := &shpyrdv1.Volume{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "data"}, vol); err != nil {
		t.Fatal(err)
	}
	vol.Status.Phase = shpyrdv1.VolumeBound
	vol.Status.MountedBy = []string{"demo/web"}
	if err := cr.Status().Update(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, "POST", "/api/projects/demo/volumes/data/snapshots", `{"name":"before"}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var snap SnapshotView
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	if snap.Name != "before" || snap.Volume != "data" || snap.Ready {
		t.Errorf("snapshot view = %+v", snap)
	}
	if rec := do(t, s, "POST", "/api/projects/demo/volumes/data/snapshots", `{"name":"before"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate snapshot: %d", rec.Code)
	}
	// Not ready yet: no restore.
	if rec := do(t, s, "POST", "/api/projects/demo/volumes/data/restore", `{"snapshot":"before"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("restore unready: %d %s", rec.Code, rec.Body.String())
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(controller.VolumeSnapshotGVK)
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "before"}, obj); err != nil {
		t.Fatal(err)
	}
	if class, _, _ := unstructured.NestedString(obj.Object, "spec", "volumeSnapshotClassName"); class != "oci-bv-backup" {
		t.Errorf("class = %q", class)
	}
	if pvc, _, _ := unstructured.NestedString(obj.Object, "spec", "source", "persistentVolumeClaimName"); pvc != "vol-data" {
		t.Errorf("source = %q", pvc)
	}
	_ = unstructured.SetNestedField(obj.Object, true, "status", "readyToUse")
	_ = unstructured.SetNestedField(obj.Object, "2Gi", "status", "restoreSize")
	if err := cr.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	rec = do(t, s, "GET", "/api/projects/demo/volumes/data/snapshots", "", true)
	var list []SnapshotView
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || len(list) != 1 || !list[0].Ready || list[0].Size != "2Gi" {
		t.Errorf("list: %d %+v", rec.Code, list)
	}
	// Another volume's snapshot list is empty: labels scope them.
	if rec := do(t, s, "GET", "/api/projects/demo/volumes/big/snapshots", "", true); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("other volume list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "POST", "/api/projects/demo/volumes/big/restore", `{"snapshot":"before"}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("restore another volume's snapshot: %d %s", rec.Code, rec.Body.String())
	}

	// Restore into a new volume: created from the snapshot, at least the
	// snapshot's size, the profile minimum applies through the controller.
	rec = do(t, s, "POST", "/api/projects/demo/volumes/data/restore", `{"snapshot":"before","to":"data-copy"}`, true)
	var res RestoreVolumeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusCreated || res.InPlace || res.Volume.Name != "data-copy" {
		t.Fatalf("restore to new: %d %s", rec.Code, rec.Body.String())
	}
	copyVol := &shpyrdv1.Volume{}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "data-copy"}, copyVol); err != nil {
		t.Fatal(err)
	}
	if copyVol.Spec.FromSnapshot != "before" || copyVol.Spec.Size.String() != "2Gi" {
		t.Errorf("copy = %+v", copyVol.Spec)
	}

	// Restore in place: the request lands as an annotation for the controller.
	rec = do(t, s, "POST", "/api/projects/demo/volumes/data/restore", `{"snapshot":"before"}`, true)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusAccepted || !res.InPlace || !strings.Contains(res.Message, "demo/web") {
		t.Fatalf("restore in place: %d %s", rec.Code, rec.Body.String())
	}
	if err := cr.Get(context.Background(), types.NamespacedName{Namespace: "app-demo", Name: "data"}, vol); err != nil {
		t.Fatal(err)
	}
	if vol.Annotations[shpyrdv1.AnnotationRestoreFrom] != "before" {
		t.Errorf("annotation = %v", vol.Annotations)
	}

	if rec := do(t, s, "DELETE", "/api/projects/demo/volumes/big/snapshots/before", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("delete through another volume: %d", rec.Code)
	}
	if rec := do(t, s, "DELETE", "/api/projects/demo/volumes/data/snapshots/before", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("delete snapshot: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "DELETE", "/api/projects/demo/volumes/data/snapshots/before", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("delete again: %d", rec.Code)
	}
}
