package install

import "testing"

func TestDefaultServerImage(t *testing.T) {
	cases := map[string]string{
		"v0.1.0":          ServerImageRepo + ":v0.1.0",
		"v1.2.3-rc.1":     ServerImageRepo + ":v1.2.3-rc.1",
		"dev":             ServerImageRepo + ":latest",
		"a81d19a-dirty":   ServerImageRepo + ":latest",
		"v0.1.0-3-gabcde": ServerImageRepo + ":latest",
		"":                ServerImageRepo + ":latest",
	}
	for v, want := range cases {
		if got := DefaultServerImage(v); got != want {
			t.Errorf("DefaultServerImage(%q) = %q, want %q", v, got, want)
		}
	}
	// Derived only when nobody set it.
	d := derivedVars(map[string]string{VarVersion: "v0.1.0", VarDomain: "x.test"}, nil)
	if d[VarServerImage] != ServerImageRepo+":v0.1.0" {
		t.Errorf("derived image = %q", d[VarServerImage])
	}
	d = derivedVars(map[string]string{VarVersion: "v0.1.0", VarDomain: "x.test", VarServerImage: "shpyrd-server:dev"}, nil)
	if _, ok := d[VarServerImage]; ok {
		t.Error("an explicit image must not be overridden")
	}
}
