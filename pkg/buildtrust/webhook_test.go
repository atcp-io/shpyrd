package buildtrust

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func buildPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-build-1-build-pod", Labels: map[string]string{"kpack.io/build": "shop-build-1"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "prepare", Env: []corev1.EnvVar{{Name: "PLATFORM_ENV_X", Value: "1"}}},
				{Name: "export", VolumeMounts: []corev1.VolumeMount{{Name: "layers-dir", MountPath: "/layers"}}},
			},
			Containers: []corev1.Container{{Name: "completion", Env: []corev1.EnvVar{{Name: EnvVar, Value: "/custom.crt"}}}},
			Volumes:    []corev1.Volume{{Name: "layers-dir"}},
		},
	}
}

// Applying the patch yields a pod where every container mounts the bundle
// and has SSL_CERT_FILE, without disturbing what was there.
func TestPatchAppliesToEveryContainer(t *testing.T) {
	pod := buildPod()
	ops := Patch(pod, "shpyrd-ca-bundle")
	raw, _ := json.Marshal(ops)
	patch, err := jsonpatch.DecodePatch(raw)
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := json.Marshal(pod)
	patched, err := patch.Apply(orig)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, raw)
	}
	var out corev1.Pod
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Spec.Volumes) != 2 || out.Spec.Volumes[1].Name != VolumeName || out.Spec.Volumes[1].ConfigMap.Name != "shpyrd-ca-bundle" {
		t.Errorf("volumes = %+v", out.Spec.Volumes)
	}
	for _, c := range append(out.Spec.InitContainers, out.Spec.Containers...) {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == VolumeName && m.MountPath == MountPath && m.ReadOnly {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("%s: bundle not mounted: %+v", c.Name, c.VolumeMounts)
		}
		var certFile string
		for _, e := range c.Env {
			if e.Name == EnvVar {
				certFile = e.Value
			}
		}
		want := MountPath + "/" + BundleKey
		if c.Name == "completion" {
			want = "/custom.crt" // an explicit value wins
		}
		if certFile != want {
			t.Errorf("%s: %s = %q, want %q", c.Name, EnvVar, certFile, want)
		}
	}
	if len(out.Spec.InitContainers[0].Env) != 2 || len(out.Spec.InitContainers[1].VolumeMounts) != 2 {
		t.Errorf("existing env/mounts must be kept: %+v", out.Spec.InitContainers)
	}
	// Idempotent: a pod that carries the volume is not patched again.
	if again := Patch(&out, "shpyrd-ca-bundle"); len(again) != 0 {
		t.Errorf("second patch = %+v", again)
	}
}

// The HTTP endpoint answers an AdmissionReview with a JSON patch.
func TestServeBuildPod(t *testing.T) {
	s, err := New(Options{Addr: ":0", TLSDir: t.TempDir(), Bundle: "shpyrd-ca-bundle"})
	if err != nil {
		t.Fatal(err)
	}
	podJSON, _ := json.Marshal(buildPod())
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request:  &admissionv1.AdmissionRequest{UID: "abc", Namespace: "app-shop", Object: runtime.RawExtension{Raw: podJSON}},
	}
	body, _ := json.Marshal(review)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/build-pod", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Response == nil || !out.Response.Allowed || out.Response.UID != "abc" || out.Response.PatchType == nil || *out.Response.PatchType != admissionv1.PatchTypeJSONPatch || len(out.Response.Patch) == 0 {
		t.Errorf("response = %+v", out.Response)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/build-pod", bytes.NewReader([]byte("nope"))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("garbage: %d", rec.Code)
	}
}
