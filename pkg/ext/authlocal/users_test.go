package authlocal

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestPasswordNameMatchesDex(t *testing.T) {
	// Dex: base32("ada@example.test" + fnv64 offset basis), lower alphabet, no padding.
	got := PasswordName("Ada@Example.test")
	if got != PasswordName("ada@example.test") {
		t.Error("names must be case-insensitive on the email")
	}
	// The name embeds the email, so it must be a valid DNS label chain and stable.
	if got == "" || strings.ContainsAny(got, "=ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("unexpected name %q", got)
	}
	// Known value: base32 of "ada@example.test" followed by the FNV-64 offset
	// basis (cbf29ce484222325), which is what dex's idToName produces.
	if want := "mfsgcqdfpbqw24dmmuxhizltotf7fhheqqrcgji"; got != want {
		t.Errorf("PasswordName = %q, want %q", got, want)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "dex.coreos.com", Version: "v1", Kind: "Password"}
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("PasswordList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{PasswordGVR: "PasswordList"})
	return &Store{Dynamic: dyn, Namespace: "shpyrd-system"}
}

func TestStoreLifecycle(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if _, err := st.Create(ctx, "not-an-email", "", "longenough"); err == nil {
		t.Error("invalid email accepted")
	}
	if _, err := st.Create(ctx, "ada@example.test", "", "short"); err == nil {
		t.Error("short password accepted")
	}
	u, err := st.Create(ctx, "Ada@Example.test", "", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "ada@example.test" || u.Name != "ada" {
		t.Errorf("user = %+v", u)
	}
	if _, err := st.Create(ctx, "ada@example.test", "", "correct horse"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: %v", err)
	}

	obj, err := st.res().Get(ctx, PasswordName("ada@example.test"), metav1GetOptions())
	if err != nil {
		t.Fatal(err)
	}
	hash, _, _ := unstructured.NestedString(obj.Object, "hash")
	if hash == "" {
		t.Fatal("hash not stored")
	}
	// The fake client keeps []byte values as base64 strings like the API server.
	raw, err := decodeHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword(raw, []byte("correct horse")) != nil {
		t.Error("stored hash does not verify the password")
	}

	if err := st.SetPassword(ctx, "ada@example.test", "battery staple"); err != nil {
		t.Fatal(err)
	}
	obj, _ = st.res().Get(ctx, PasswordName("ada@example.test"), metav1GetOptions())
	hash2, _, _ := unstructured.NestedString(obj.Object, "hash")
	raw2, _ := decodeHash(hash2)
	if bcrypt.CompareHashAndPassword(raw2, []byte("battery staple")) != nil {
		t.Error("password change not applied")
	}

	users, err := st.List(ctx)
	if err != nil || len(users) != 1 || users[0].Email != "ada@example.test" {
		t.Errorf("list = %+v, %v", users, err)
	}
	if err := st.Delete(ctx, "ada@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "ada@example.test"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("second delete: %v", err)
	}
}

func metav1GetOptions() metav1.GetOptions { return metav1.GetOptions{} }

func decodeHash(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
