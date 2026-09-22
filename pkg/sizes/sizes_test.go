package sizes

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestDefaultsValidAndRoundTrip(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Sizes) < 8 {
		t.Errorf("want at least 8 sizes, got %d", len(c.Sizes))
	}
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Default != "shared-s" || len(back.Sizes) != len(c.Sizes) {
		t.Errorf("round trip changed the catalog: %+v", back)
	}
}

func TestResources(t *testing.T) {
	s := Size{Name: "shared-s", Kind: Shared, CPU: "0.5", Memory: "64Mi"}
	r := s.Resources()
	if r.Requests.Cpu().String() != "500m" || r.Limits.Cpu().String() != "2" || r.Limits.Memory().String() != "64Mi" || r.Requests.Memory().String() != "64Mi" {
		t.Errorf("shared resources = %+v", r)
	}
	d := Size{Name: "dedicated-m", Kind: Dedicated, CPU: "2", Memory: "4Gi"}
	r = d.Resources()
	if r.Requests.Cpu().String() != "2" || r.Limits.Cpu().String() != "2" || r.Limits.Memory().String() != "4Gi" {
		t.Errorf("dedicated resources = %+v", r)
	}
}

func TestResolve(t *testing.T) {
	c := Defaults()
	r, name, err := c.Resolve("", corev1.ResourceRequirements{})
	if err != nil || name != "shared-s" || r.Requests.Cpu().String() != "500m" {
		t.Errorf("default resolve = %v %q %+v", err, name, r)
	}
	if _, _, err := c.Resolve("nope", corev1.ResourceRequirements{}); err == nil {
		t.Error("unknown size must fail")
	}
	// Explicit memory override keeps the size's cpu.
	r, name, err = c.Resolve("shared-m", corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}})
	if err != nil || name != "" || r.Limits.Memory().String() != "1Gi" || r.Requests.Memory().String() != "1Gi" || r.Requests.Cpu().String() != "500m" {
		t.Errorf("override resolve = %v %q %+v", err, name, r)
	}
}

func TestUpsertRemove(t *testing.T) {
	c := Defaults()
	if err := c.Upsert(Size{Name: "huge", Kind: Dedicated, CPU: "32", Memory: "64Gi"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("huge"); !ok {
		t.Error("upsert did not add")
	}
	if err := c.Remove("shared-s"); err == nil {
		t.Error("removing the default must fail")
	}
	if err := c.Remove("huge"); err != nil {
		t.Error(err)
	}
	if err := c.Upsert(Size{Name: "Bad", Kind: Shared, CPU: "1", Memory: "1Gi"}); err == nil {
		t.Error("invalid name must fail")
	}
	if err := (Catalog{Default: "x", Sizes: []Size{{Name: "a", Kind: Shared, CPU: "1", Memory: "1Gi"}}}).Validate(); err == nil {
		t.Error("default not in catalog must fail")
	}
}
