package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shpyrdv1 "shpyrd/api/v1alpha1"
	"shpyrd/pkg/logs"
)

// The web terminal (RFC-0026): a shell into a running instance from the
// dashboard. The browser opens a WebSocket, the server bridges it to
// pods/exec with a TTY, and the `project.exec` role gates the whole feature.

// runProcess is the process label of one-off pods (RFC-0024), which share the
// project's namespace and are not shellable. The controller keeps its own
// unexported copy; a third caller should move this to api/v1alpha1 rather
// than copy it again.
const runProcess = "run"

// appContainer is the container a project's process runs in.
const appContainer = "app"

// Instance is a running instance of a project, named the way the log viewer
// names it.
type Instance struct {
	Name    string `json:"name"`    // web.1
	Process string `json:"process"` // web
	Pod     string `json:"pod"`
	Ready   bool   `json:"ready"`
}

// listInstances lists what a shell can attach to: running app pods, never
// build or run pods (RFC-0026 non-goals).
func (s *Server) listInstances(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	out, err := s.instancesOf(c, app.Namespace, app.Name)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// instancesOf lists a project's shellable instances. Names are computed over
// the filtered set, which is safe because InstanceNames numbers within a
// process: dropping run pods cannot renumber web.1.
func (s *Server) instancesOf(c *gin.Context, namespace, app string) ([]Instance, error) {
	pods, err := s.kube.Kube.CoreV1().Pods(namespace).List(c.Request.Context(), metav1.ListOptions{
		// "!=" also matches pods carrying no process label at all.
		LabelSelector: shpyrdv1.LabelApp + "=" + app + "," + shpyrdv1.LabelProcess + "!=" + runProcess,
	})
	if err != nil {
		return nil, err
	}
	var running []corev1.Pod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	names := logs.InstanceNames(running)
	out := make([]Instance, 0, len(running))
	for _, p := range running {
		out = append(out, Instance{
			Name:    names[p.Name],
			Process: p.Labels[shpyrdv1.LabelProcess],
			Pod:     p.Name,
			Ready:   podReady(p),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
