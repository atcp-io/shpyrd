// Package logsagent is the logs-agent extension (RFC-0022a): a Vector
// DaemonSet that collects container logs from all project pods, attaches
// project/process/instance labels and feeds log drains (RFC-0023).
// The extension has no API routes or CLI commands beyond enable/disable.
package logsagent

import (
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/shpyrd-io/shpyrd/pkg/ext"
)

const Name = "logs-agent"

type extension struct{}

func New() ext.Extension { return extension{} }

func (extension) Name() string { return Name }
func (extension) Description() string {
	return "Vector log agent: collects container logs from all project pods, labels them project/process/instance, feeds drains (RFC-0022a)"
}
func (extension) Components() []ext.ComponentRef {
	return []ext.ComponentRef{{Name: "logs-agent", Runlevel: "rc3"}}
}
func (extension) Register(ctrl.Manager, ext.Deps) error { return nil }
func (extension) Routes(_ ext.Router, _ ext.Deps) error { return nil }
func (extension) CLI(_ ext.CLIGlobals) []*cobra.Command { return nil }
func (extension) Types() []ext.ResourceType             { return nil }
