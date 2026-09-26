// Package store is the platform's control-plane database (RFC-0033): the
// workspace, the people it has seen, teams and their grants on projects.
// Workloads stay Kubernetes objects; what is about people and tenancy lives
// here, in Postgres on a real install and in memory in tests.
package store

import (
	"context"
	"errors"
	"time"
)

// DefaultWorkspace is the slug of the implicit workspace every open-source
// install has: names built from it carry no workspace part.
const DefaultWorkspace = "default"

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// Workspace is the tenant. The OSS has exactly one.
type Workspace struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Identity is a person the platform has seen sign in: recorded at every
// sign-in so the workspace has a list of its people before RFC-0033's
// invitations and provisioning arrive.
type Identity struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	Realm       string    `json:"realm"` // workspace, console, operator
	Email       string    `json:"email"`
	Name        string    `json:"name,omitempty"`
	Provider    string    `json:"provider,omitempty"` // last login method
	Groups      []string  `json:"groups,omitempty"`   // last groups claim
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
}

// Team groups people by email or by identity-provider group; a team may
// carry a platform role (RFC-0008) until workspace roles replace it.
type Team struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspaceId"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Members      []string  `json:"members"` // emails, lower case
	Groups       []string  `json:"groups"`
	PlatformRole string    `json:"platformRole,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Grant gives a role on a project to a user (email) or to a team; exactly
// one of the two is set.
type Grant struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	Project     string    `json:"project"`
	Role        string    `json:"role"`
	User        string    `json:"user,omitempty"`
	Team        string    `json:"team,omitempty"` // team name
	CreatedAt   time.Time `json:"createdAt"`
}

// Store is what the server and the controllers need. Every method is
// scoped to a workspace by slug; the OSS passes DefaultWorkspace.
type Store interface {
	// Migrate brings the schema to the current version and ensures the
	// implicit workspace exists.
	Migrate(ctx context.Context, defaultName string) error
	Close()

	Workspace(ctx context.Context, slug string) (*Workspace, error)
	UpdateWorkspace(ctx context.Context, slug, name string) (*Workspace, error)

	// TouchIdentity records a sign-in: creates the person on first sight,
	// updates name, provider, groups and the time otherwise.
	TouchIdentity(ctx context.Context, ws string, id Identity) (*Identity, error)
	ListIdentities(ctx context.Context, ws string) ([]Identity, error)
	DeleteIdentity(ctx context.Context, ws, email string) error

	ListTeams(ctx context.Context, ws string) ([]Team, error)
	GetTeam(ctx context.Context, ws, name string) (*Team, error)
	// PutTeam creates or replaces a team by name; created reports which.
	PutTeam(ctx context.Context, ws string, t Team) (team *Team, created bool, err error)
	// DeleteTeam removes the team and every grant given to it.
	DeleteTeam(ctx context.Context, ws, name string) error

	ListGrants(ctx context.Context, ws string) ([]Grant, error)
	ListProjectGrants(ctx context.Context, ws, project string) ([]Grant, error)
	// AddGrant fails with ErrConflict when the same grant exists and with
	// ErrNotFound when the team does not.
	AddGrant(ctx context.Context, ws string, g Grant) (*Grant, error)
	DeleteGrant(ctx context.Context, ws, id string) error
	// DeleteProjectGrants removes every grant of a project (project destroyed).
	DeleteProjectGrants(ctx context.Context, ws, project string) error

	// Export and Import move the whole workspace's people and tenancy
	// (platform backups, RFC-0037).
	Export(ctx context.Context, ws string) (*Dump, error)
	Import(ctx context.Context, ws string, d *Dump, overwrite bool) (*ImportResult, error)
}

// Dump is a workspace's content as the backup carries it.
type Dump struct {
	Version    int        `json:"version"`
	Workspace  Workspace  `json:"workspace"`
	Identities []Identity `json:"identities"`
	Teams      []Team     `json:"teams"`
	Grants     []Grant    `json:"grants"`
}

// ImportResult counts what Import did.
type ImportResult struct {
	Teams, Grants, Identities int
	Skipped                   int
}

// DumpVersion is the format of Dump.
const DumpVersion = 1
