// Package all is the registry of built-in extensions (RFC-0002). Enabling
// is data (the install record), not a build tag: one binary serves every
// cluster.
package all

import (
	"strings"

	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/ext/authlocal"
	"github.com/shpyrd-io/shpyrd/pkg/ext/authoidc"
	"github.com/shpyrd-io/shpyrd/pkg/ext/logsagent"
	"github.com/shpyrd-io/shpyrd/pkg/ext/objectstorage"
	"github.com/shpyrd-io/shpyrd/pkg/ext/postgres"
	"github.com/shpyrd-io/shpyrd/pkg/ext/redis"
)

// All lists every extension the binaries know about, in display order.
func All() []ext.Extension {
	return []ext.Extension{
		authlocal.New(),
		logsagent.New(),
		authoidc.New(),
		postgres.New(),
		redis.New(),
		objectstorage.New(),
	}
}

// Enabled resolves a comma separated SHPYRD_EXTENSIONS value; unknown names
// are returned separately so callers can warn.
func Enabled(csv string) (enabled []ext.Extension, unknown []string) {
	for _, name := range splitCSV(csv) {
		if x := ext.Find(All(), name); x != nil {
			enabled = append(enabled, x)
		} else {
			unknown = append(unknown, name)
		}
	}
	return enabled, unknown
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// BindableTypes lists the resource kinds of the given extensions that apps
// can attach.
func BindableTypes(list []ext.Extension) []ext.ResourceType {
	var out []ext.ResourceType
	for _, x := range list {
		for _, t := range x.Types() {
			if t.Bindable {
				out = append(out, t)
			}
		}
	}
	return out
}
