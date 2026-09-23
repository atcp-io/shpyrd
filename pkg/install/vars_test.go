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

func TestFrontDoorVars(t *testing.T) {
	kind := map[string]string{VarDomain: "127.0.0.1.nip.io", VarHTTPSPort: "8443", VarFrontDoor: FrontDoorKind}
	if u := BaseURL(kind)("shpyrd"); u != "https://shpyrd.127.0.0.1.nip.io:8443" {
		t.Errorf("kind mode URL = %s", u)
	}
	d := derivedVars(kind, nil)
	if d[VarURLPort] != "8443" || d[VarForwardedHeaders] != "false" || d[VarAuthURL] != "https://auth.127.0.0.1.nip.io:8443" {
		t.Errorf("kind mode derived = %v", d)
	}
	caddy := map[string]string{VarDomain: "shpyrd.test", VarHTTPPort: "8080", VarHTTPSPort: "8443", VarFrontDoor: FrontDoorCaddy}
	if u := BaseURL(caddy)("shpyrd"); u != "https://shpyrd.shpyrd.test" {
		t.Errorf("caddy mode URL = %s", u)
	}
	d = derivedVars(caddy, nil)
	if d[VarURLPort] != "443" || d[VarForwardedHeaders] != "true" || d[VarDashboardURL] != "https://shpyrd.shpyrd.test" {
		t.Errorf("caddy mode derived = %v", d)
	}
	if URLPort(map[string]string{}) != "443" {
		t.Error("URLPort default")
	}
}
