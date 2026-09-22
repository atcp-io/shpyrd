// Package ui embeds the built single page application (ui/dist) into the
// server binary. Run `make ui` to populate dist before building.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built UI rooted at index.html. When the UI has not been
// built the file system only contains a .gitkeep and the server serves a
// placeholder page instead.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
