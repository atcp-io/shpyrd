package controller

import (
	"testing"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
)

// Releases that leave the history hand their images to StaleImages unless a
// kept release (a config-only release shares its predecessor's image) still
// uses them.
func TestUnreferencedImages(t *testing.T) {
	dropped := []shpyrdv1.Release{{Number: 1, Image: "r/a@sha256:1"}, {Number: 2, Image: "r/a@sha256:2"}, {Number: 3, Image: "r/a@sha256:2"}}
	kept := []shpyrdv1.Release{{Number: 4, Image: "r/a@sha256:2"}, {Number: 5, Image: "r/a@sha256:3"}}
	got := unreferencedImages(dropped, kept)
	if len(got) != 1 || got[0] != "r/a@sha256:1" {
		t.Errorf("unreferenced = %v", got)
	}
}
