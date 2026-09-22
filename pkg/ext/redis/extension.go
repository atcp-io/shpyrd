// Package redis is the redis extension (RFC-0010): Redis-compatible stores
// (Valkey by default) for projects, attachable to apps as REDIS_URL.
package redis

import (
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/internal/controller"
	"shpyrd/pkg/ext"
)

// Name of the extension.
const Name = "redis"

type extension struct{}

// New returns the extension.
func New() ext.Extension { return extension{} }

func (extension) Name() string { return Name }
func (extension) Description() string {
	return "Redis-compatible caches and queues for projects (Valkey or Redis), attached to apps as REDIS_URL (shpyrd redis create)"
}

// Component is nil: the controller runs the engine itself, no operator needed.
func (extension) Component() *ext.ComponentRef { return nil }

// Register runs the Redis controller and makes the kind attachable.
func (extension) Register(mgr ctrl.Manager, deps ext.Deps) error {
	r := &controller.RedisReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("shpyrd"), SystemNamespace: deps.SystemNamespace}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	controller.RegisterBinder("Redis", controller.RedisBinder{})
	return nil
}

func (extension) Routes(ext.Router, ext.Deps) error { return nil }

func (extension) Types() []ext.ResourceType {
	return []ext.ResourceType{{Kind: "Redis", Group: shpyrdv1.GroupVersion.Group, Version: shpyrdv1.GroupVersion.Version, Resource: "redis", Bindable: true}}
}

// CLI returns `shpyrd redis`.
func (extension) CLI(g ext.CLIGlobals) []*cobra.Command { return []*cobra.Command{newRedisCmd(g)} }
