// Package deploy embeds the base stack manifests: Kustomize components, Helm
// values and installer profiles. The install engine renders them in-process.
package deploy

import "embed"

// FS contains components/ and profiles/.
//
//go:embed all:components all:profiles
var FS embed.FS
