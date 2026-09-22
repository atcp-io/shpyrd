package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// LogLine is one log line in the NDJSON stream.
type LogLine struct {
	Time     string `json:"t,omitempty"`
	Instance string `json:"i"` // Heroku style: web.1, worker.2
	Pod      string `json:"p"`
	Message  string `json:"m"`
}

// InstanceNames maps pods to stable, human names ("web.1", "web.2") by
// process type and creation order, the way Heroku names dynos.
func InstanceNames(pods []corev1.Pod) map[string]string {
	byProc := map[string][]corev1.Pod{}
	for _, p := range pods {
		byProc[p.Labels[shpyrdv1.LabelProcess]] = append(byProc[p.Labels[shpyrdv1.LabelProcess]], p)
	}
	out := map[string]string{}
	for proc, list := range byProc {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].CreationTimestamp.Equal(&list[j].CreationTimestamp) {
				return list[i].CreationTimestamp.Before(&list[j].CreationTimestamp)
			}
			return list[i].Name < list[j].Name
		})
		for i, p := range list {
			name := proc
			if name == "" {
				name = "app"
			}
			out[p.Name] = fmt.Sprintf("%s.%d", name, i+1)
		}
	}
	return out
}

// splitTimestamp separates the RFC3339Nano prefix kubelet adds with
// Timestamps=true from the message.
func splitTimestamp(line string) (string, string) {
	if i := strings.IndexByte(line, ' '); i > 0 {
		if _, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
			return line[:i], line[i+1:]
		}
	}
	return "", line
}

// appLogs streams pod logs. ?process=web filters a process type, ?tail=N
// limits history, ?follow=true keeps streaming, ?format=json emits NDJSON
// LogLine objects; the default is text "<time> <instance> | <message>".
func (s *Server) appLogs(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	selector := shpyrdv1.LabelApp + "=" + app.Name + "," + shpyrdv1.LabelProcess
	if p := c.Query("process"); p != "" {
		selector = shpyrdv1.LabelApp + "=" + app.Name + "," + shpyrdv1.LabelProcess + "=" + p
	}
	tail := int64(200)
	if t, err := strconv.ParseInt(c.Query("tail"), 10, 64); err == nil {
		tail = t
	}
	follow := c.Query("follow") == "true" || c.Query("follow") == "1"
	asJSON := c.Query("format") == "json"

	pods, err := s.kube.Kube.CoreV1().Pods(app.Namespace).List(c.Request.Context(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	if len(pods.Items) == 0 {
		abort(c, http.StatusNotFound, errors.New("no running pods for this app"))
		return
	}
	names := InstanceNames(pods.Items)

	if asJSON {
		c.Header("Content-Type", "application/x-ndjson; charset=utf-8")
	} else {
		c.Header("Content-Type", "text/plain; charset=utf-8")
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	w := newLineWriter(c.Writer)
	emit := func(l LogLine) {
		if asJSON {
			b, _ := json.Marshal(l)
			w.line(string(b))
			return
		}
		w.line(fmt.Sprintf("%s %s | %s", l.Time, l.Instance, l.Message))
	}

	ctx := c.Request.Context()
	if follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
	}
	var wg sync.WaitGroup
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodPending {
			continue
		}
		wg.Add(1)
		go func(pod corev1.Pod) {
			defer wg.Done()
			opts := &corev1.PodLogOptions{Container: "app", Follow: follow, Timestamps: true}
			if tail >= 0 {
				opts.TailLines = &tail
			}
			stream, err := s.kube.Kube.CoreV1().Pods(app.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
			if err != nil {
				emit(LogLine{Instance: names[pod.Name], Pod: pod.Name, Message: "(no logs: " + err.Error() + ")"})
				return
			}
			defer stream.Close()
			sc := bufio.NewScanner(stream)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				ts, msg := splitTimestamp(sc.Text())
				emit(LogLine{Time: ts, Instance: names[pod.Name], Pod: pod.Name, Message: msg})
			}
		}(pod)
	}
	wg.Wait()
}

// ---- builds -----------------------------------------------------------------

var kpackBuildGVK = schema.GroupVersionKind{Group: "kpack.io", Version: "v1alpha2", Kind: "Build"}

// BuildInfo summarises a kpack Build.
type BuildInfo struct {
	Name        string     `json:"name"`
	Number      int        `json:"number"`
	Status      string     `json:"status"` // Building, Succeeded, Failed
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	Digest      string     `json:"digest,omitempty"`
	Source      string     `json:"source,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Steps       []string   `json:"stepsCompleted,omitempty"`
}

func buildInfo(u unstructured.Unstructured) BuildInfo {
	b := BuildInfo{Name: u.GetName(), Status: "Building", StartedAt: u.GetCreationTimestamp().Time}
	b.Number, _ = strconv.Atoi(u.GetLabels()["image.kpack.io/buildNumber"])
	b.Reason = u.GetAnnotations()["image.kpack.io/reason"]
	img, _, _ := unstructured.NestedString(u.Object, "status", "latestImage")
	b.Digest = Digest(img)
	if rev, _, _ := unstructured.NestedString(u.Object, "spec", "source", "git", "revision"); rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		b.Source = rev
	} else if _, found, _ := unstructured.NestedString(u.Object, "spec", "source", "blob", "url"); found {
		b.Source = "archive"
	}
	steps, _, _ := unstructured.NestedStringSlice(u.Object, "status", "stepsCompleted")
	b.Steps = steps
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, raw := range conds {
		m, ok := raw.(map[string]interface{})
		if !ok || m["type"] != "Succeeded" {
			continue
		}
		switch m["status"] {
		case "True":
			b.Status = "Succeeded"
		case "False":
			b.Status = "Failed"
		}
		if msg, ok := m["message"].(string); ok {
			b.Message = msg
		}
		if b.Status != "Building" {
			if lt, ok := m["lastTransitionTime"].(string); ok {
				if t, err := time.Parse(time.RFC3339, lt); err == nil {
					b.CompletedAt = &t
				}
			}
		}
	}
	return b
}

func (s *Server) listBuilds(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kpack.io", Version: "v1alpha2", Kind: "BuildList"})
	if err := s.apps.List(c.Request.Context(), list, client.InNamespace(app.Namespace), client.MatchingLabels{"image.kpack.io/image": app.Name}); err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	out := make([]BuildInfo, 0, len(list.Items))
	for _, u := range list.Items {
		out = append(out, buildInfo(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	c.JSON(http.StatusOK, out)
}

// buildLogs streams the steps of one build (or the latest with name
// "latest"). With ?follow=true it waits for steps to start and keeps
// streaming until the build pod finishes.
func (s *Server) buildLogs(c *gin.Context) {
	app, ok := s.loadApp(c)
	if !ok {
		return
	}
	build := c.Param("build")
	if build == "" || build == "latest" {
		build = app.Status.LatestBuild
	}
	if build == "" {
		abort(c, http.StatusNotFound, errors.New("no build yet"))
		return
	}
	follow := c.Query("follow") == "true" || c.Query("follow") == "1"
	podName := build + "-build-pod"
	pods := s.kube.Kube.CoreV1().Pods(app.Namespace)
	ctx := c.Request.Context()
	if follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
	}

	// Wait for the pod when following (kpack creates it a moment after the Build).
	var pod *corev1.Pod
	for {
		p, err := pods.Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			pod = p
			break
		}
		if !apierrors.IsNotFound(err) {
			abort(c, http.StatusBadGateway, err)
			return
		}
		if !follow {
			abort(c, http.StatusNotFound, fmt.Errorf("build pod %s is gone (kpack keeps a limited history)", podName))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	w := newLineWriter(c.Writer)

	for _, ic := range pod.Spec.InitContainers {
		if follow {
			// Block until the step starts (or the pod fails before it).
			for {
				p, err := pods.Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return
				}
				st := initStatus(p, ic.Name)
				if st != nil && (st.State.Running != nil || st.State.Terminated != nil) {
					break
				}
				if p.Status.Phase == corev1.PodFailed {
					w.line("===> build pod failed before step " + ic.Name)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
		w.line("===> " + ic.Name)
		stream, err := pods.GetLogs(podName, &corev1.PodLogOptions{Container: ic.Name, Follow: follow}).Stream(ctx)
		if err != nil {
			w.line("(not started)")
			if follow {
				return
			}
			continue
		}
		_, _ = io.Copy(w, stream)
		stream.Close()
		if p, err := pods.Get(ctx, podName, metav1.GetOptions{}); err == nil {
			if st := initStatus(p, ic.Name); st != nil && st.State.Terminated != nil && st.State.Terminated.ExitCode != 0 {
				w.line(fmt.Sprintf("===> step %s failed (exit %d)", ic.Name, st.State.Terminated.ExitCode))
				return
			}
		}
	}
	if follow {
		w.line("===> done")
	}
}

func initStatus(p *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range p.Status.InitContainerStatuses {
		if p.Status.InitContainerStatuses[i].Name == name {
			return &p.Status.InitContainerStatuses[i]
		}
	}
	return nil
}

// lineWriter serialises concurrent writers and flushes after each line so
// clients see log lines as they arrive.
type lineWriter struct {
	mu sync.Mutex
	w  gin.ResponseWriter
}

func newLineWriter(w gin.ResponseWriter) *lineWriter { return &lineWriter{w: w} }

func (l *lineWriter) line(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, s+"\n")
	l.w.Flush()
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.w.Write(p)
	l.w.Flush()
	return n, err
}

var _ = kpackBuildGVK

// buildsByDigest maps image digests to kpack build numbers for this app.
func (s *Server) buildsByDigest(ctx context.Context, app *shpyrdv1.App) map[string]int {
	out := map[string]int{}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kpack.io", Version: "v1alpha2", Kind: "BuildList"})
	if err := s.apps.List(ctx, list, client.InNamespace(app.Namespace), client.MatchingLabels{"image.kpack.io/image": app.Name}); err != nil {
		return out
	}
	for _, u := range list.Items {
		b := buildInfo(u)
		if b.Digest != "" {
			out[b.Digest] = b.Number
		}
	}
	return out
}
