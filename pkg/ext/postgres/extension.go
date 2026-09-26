// Package postgres is the postgres extension (RFC-0009): PostgreSQL databases
// for projects, run by the CloudNativePG operator, attachable to apps as
// DATABASE_URL.
package postgres

import (
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	shpyrdv1 "github.com/shpyrd-io/shpyrd/api/v1alpha1"
	"github.com/shpyrd-io/shpyrd/internal/controller"
	"github.com/shpyrd-io/shpyrd/pkg/ext"
	"github.com/shpyrd-io/shpyrd/pkg/install"
)

// Name of the extension.
const Name = "postgres"

type extension struct{}

// New returns the extension.
func New() ext.Extension { return extension{} }

func (extension) Name() string { return Name }
func (extension) Description() string {
	return "PostgreSQL databases for projects (CloudNativePG), attached to apps as DATABASE_URL (shpyrd pg create)"
}
func (extension) Components() []ext.ComponentRef {
	return []ext.ComponentRef{{Name: "cnpg", Runlevel: "rc2"}, {Name: "barman-cloud", Runlevel: "rc3"}}
}

// Register runs the Postgres controller and makes the kind attachable.
func (extension) Register(mgr ctrl.Manager, deps ext.Deps) error {
	r := &controller.PostgresReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("shpyrd"), SystemNamespace: deps.SystemNamespace, Storage: controller.StorageProfile{Class: deps.Var(install.VarStorageClass), MinSize: deps.Var(install.VarVolumeMinSize)}}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	controller.RegisterBinder("Postgres", controller.PostgresBinder{})
	return nil
}

func (extension) Routes(ext.Router, ext.Deps) error { return nil }

func (extension) Types() []ext.ResourceType {
	return []ext.ResourceType{{Kind: "Postgres", Group: shpyrdv1.GroupVersion.Group, Version: shpyrdv1.GroupVersion.Version, Resource: "postgres", Bindable: true}}
}

// CLI returns `shpyrd pg`.
func (extension) CLI(g ext.CLIGlobals) []*cobra.Command { return []*cobra.Command{newPgCmd(g)} }
