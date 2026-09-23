package authoidc

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"shpyrd/pkg/ext"
)

type extension struct{}

// New returns the extension.
func New() ext.Extension { return extension{} }

func (extension) Name() string { return Name }
func (extension) Description() string {
	return "Sign in with a company identity provider: Okta or any OpenID Connect issuer (shpyrd auth oidc set)"
}

// Component is nil: the extension is configuration held in Secrets.
func (extension) Component() *ext.ComponentRef          { return nil }
func (extension) Register(ctrl.Manager, ext.Deps) error { return nil }
func (extension) Types() []ext.ResourceType             { return nil }
func (extension) CLI(g ext.CLIGlobals) []*cobra.Command { return []*cobra.Command{newAuthCmd(g)} }

// Routes registers every configured provider with the relying party.
// Discovery needs the network, so it runs in the background with retries
// and the server starts regardless.
func (extension) Routes(_ ext.Router, deps ext.Deps) error {
	if deps.Auth == nil || deps.Kube == nil || deps.Kube.Kube == nil {
		return nil
	}
	store := &Store{Kube: deps.Kube.Kube, Namespace: deps.SystemNamespace}
	go registerAll(context.Background(), deps, store)
	return nil
}

func registerAll(ctx context.Context, deps ext.Deps, store *Store) {
	pending := map[string]Provider{}
	list, err := store.List(ctx)
	if err != nil {
		fmt.Printf("auth-oidc: cannot list providers (%v); retrying\n", err)
	}
	for _, p := range list {
		pending[p.ID] = p
	}
	delay := 2 * time.Second
	for attempt := 1; ; attempt++ {
		if err == nil && len(pending) == 0 {
			return
		}
		if err != nil {
			if list, err = store.List(ctx); err == nil {
				for _, p := range list {
					pending[p.ID] = p
				}
			}
		}
		for id, p := range pending {
			if rerr := deps.Auth.AddOIDC(ctx, p.OIDC()); rerr != nil {
				if attempt == 1 || attempt%10 == 0 {
					fmt.Printf("auth-oidc: provider %s not ready (%v); retrying\n", id, rerr)
				}
				continue
			}
			delete(pending, id)
		}
		if err == nil && len(pending) == 0 {
			return
		}
		if delay < 30*time.Second {
			delay *= 2
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}
