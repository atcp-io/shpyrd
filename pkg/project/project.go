// Package project holds the naming rules of a project (RFC-0011): a human
// display name, a URL-safe slug derived from it, and the namespace scheme.
package project

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// MaxSlugLength keeps hostnames (<slug>.<domain>) and namespaces
// (app-<slug>) within DNS label limits.
const MaxSlugLength = 40

var slugRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// ValidSlug reports whether s is a slug as produced by Slug.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// Slug derives the identifier of a project from its display name:
// lowercase ASCII letters and digits separated by single dashes, accents
// stripped, at most MaxSlugLength characters. It returns an error when
// nothing usable is left.
func Slug(displayName string) (string, error) {
	var b strings.Builder
	dash := true // suppress leading dashes
	for _, r := range norm.NFD.String(displayName) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue // combining mark left by NFD (accent)
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(unicode.ToLower(r))
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := b.String()
	if len(s) > MaxSlugLength {
		s = s[:MaxSlugLength]
	}
	s = strings.TrimRight(s, "-")
	s = strings.TrimPrefix(s, "app-") // the namespace prefix is not a valid slug start
	s = strings.TrimLeft(s, "-")
	if s == "" {
		return "", fmt.Errorf("cannot derive a slug from %q: use at least one letter or digit", displayName)
	}
	return s, nil
}

// ValidateSlug explains why a slug given explicitly is not acceptable.
// Slugs never start with "app-", the namespace prefix, so a namespace can
// never be mistaken for a slug.
func ValidateSlug(s string) error {
	if !ValidSlug(s) {
		return fmt.Errorf("invalid slug %q: use lowercase letters, digits and dashes (max %d characters)", s, MaxSlugLength)
	}
	if strings.HasPrefix(s, "app-") {
		return fmt.Errorf("invalid slug %q: slugs cannot start with \"app-\"", s)
	}
	return nil
}

// reservedHosts are the first labels the platform keeps for itself under
// any domain it serves (RFC-0033): a project or workspace with one of these
// slugs would answer at a host that belongs to the platform, or invite
// phishing (login.<domain>). Existing objects are never renamed; the check
// applies to what is created from now on.
var reservedHosts = map[string]bool{
	"www": true, "api": true, "app": true, "apps": true, "auth": true, "login": true,
	"signin": true, "console": true, "ops": true, "admin": true, "mail": true, "smtp": true,
	"shpyrd": true, "status": true, "docs": true, "grafana": true, "registry": true,
	"edge": true, "oauth": true, "well-known": true, "ns1": true, "ns2": true,
}

// Reserved reports whether a slug is kept for the platform's own hosts.
func Reserved(slug string) bool { return reservedHosts[slug] }

// ValidateNewSlug is ValidateSlug plus the reserved names: for creation
// only, so projects that already carry one of these names keep working.
func ValidateNewSlug(s string) error {
	if err := ValidateSlug(s); err != nil {
		return err
	}
	if Reserved(s) {
		return fmt.Errorf("%q is reserved for the platform; pick another name", s)
	}
	return nil
}

// DefaultWorkspace is the slug of the implicit workspace of the open-source
// platform (RFC-0033); names built from it carry no workspace part.
const DefaultWorkspace = "default"

// MaxWorkspaceSlugLength keeps app-<workspace>-<project> under 63 characters.
const MaxWorkspaceSlugLength = 24

var workspaceSlugRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,22}[a-z0-9])?$`)

// ValidateWorkspaceSlug explains why a workspace slug is not acceptable: the
// same alphabet as project slugs, at most MaxWorkspaceSlugLength characters,
// never a reserved host and never "default", which names the implicit one.
func ValidateWorkspaceSlug(s string) error {
	if !workspaceSlugRe.MatchString(s) {
		return fmt.Errorf("invalid workspace slug %q: use lowercase letters, digits and dashes (max %d characters)", s, MaxWorkspaceSlugLength)
	}
	if s == DefaultWorkspace || Reserved(s) || strings.HasPrefix(s, "app-") {
		return fmt.Errorf("%q is reserved; pick another workspace slug", s)
	}
	return nil
}

// Namespace of a project in the implicit workspace.
func Namespace(slug string) string { return NamespaceIn(DefaultWorkspace, slug) }

// NamespaceIn is the only place namespace names are built (RFC-0033): the
// implicit workspace keeps app-<project>; any other gives
// app-<workspace>-<project>. Names are never parsed back: the labels
// shpyrd.io/workspace and shpyrd.io/project are authoritative.
func NamespaceIn(workspace, slug string) string {
	if workspace == "" || workspace == DefaultWorkspace {
		return "app-" + slug
	}
	return "app-" + workspace + "-" + slug
}

// NamespaceLabels are the labels every project namespace carries.
func NamespaceLabels(workspace, slug string) map[string]string {
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	return map[string]string{
		shpyrdv1.LabelApp:       slug,
		shpyrdv1.LabelProject:   slug,
		shpyrdv1.LabelWorkspace: workspace,
		shpyrdv1.LabelManagedBy: "shpyrd",
	}
}

// FromNamespace maps app-<slug> to <slug>.
func FromNamespace(ns string) string { return strings.TrimPrefix(ns, "app-") }

// DisplayName of an App: its annotation, or the slug.
func DisplayName(a *shpyrdv1.App) string {
	if a == nil {
		return ""
	}
	if n := strings.TrimSpace(a.Annotations[shpyrdv1.AnnotationDisplayName]); n != "" {
		return n
	}
	return a.Name
}

// SetDisplayName records the display name on the App, dropping the
// annotation when it adds nothing over the slug.
func SetDisplayName(a *shpyrdv1.App, name string) {
	name = strings.TrimSpace(name)
	if name == "" || name == a.Name {
		delete(a.Annotations, shpyrdv1.AnnotationDisplayName)
		return
	}
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	a.Annotations[shpyrdv1.AnnotationDisplayName] = name
}

// Label formats a project for people: "My Shop (my-shop)", or just the
// slug when the two coincide.
func Label(a *shpyrdv1.App) string {
	n := DisplayName(a)
	if n == a.Name {
		return n
	}
	return n + " (" + a.Name + ")"
}
