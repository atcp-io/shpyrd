// Package buildtrust serves the admission webhook that makes kpack build
// pods trust the platform CA (RFC-0059). kpack has no setting for a private
// CA and every step of a build (build-init, the lifecycle, completion)
// talks to the registry, so the pod is patched on creation: the cluster's
// trust bundle (trust-manager's ConfigMap, present in every namespace) is
// mounted into each container and SSL_CERT_FILE points at it, which every
// Go binary honours.
package buildtrust

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// VolumeName is the pod volume the bundle is mounted from.
	VolumeName = "shpyrd-ca"
	// MountPath is where the bundle lands in every container.
	MountPath = "/etc/shpyrd/ca"
	// BundleKey is the file inside the ConfigMap (trust-manager's target key).
	BundleKey = "ca-certificates.crt"
	// EnvVar is the Go runtime variable naming the root certificates file.
	EnvVar = "SSL_CERT_FILE"
)

// Options configure the webhook server.
type Options struct {
	// Addr is the TLS listen address (":9443").
	Addr string
	// TLSDir holds tls.crt and tls.key (a cert-manager Secret mount); the
	// certificate is re-read when it changes on disk so renewals apply.
	TLSDir string
	// Bundle is the trust bundle ConfigMap name in the pod's namespace.
	Bundle string
	Logger *slog.Logger
}

// Server is the webhook HTTP server.
type Server struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	loadedAt time.Time
}

// New returns a webhook server; Run starts it.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" || opts.TLSDir == "" {
		return nil, errors.New("webhook: address and TLS directory are required")
	}
	if opts.Bundle == "" {
		opts.Bundle = "shpyrd-ca-bundle"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{opts: opts, log: opts.Logger}, nil
}

// Handler serves the admission endpoint (exposed for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/build-pod", s.serveBuildPod)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// Run serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.certificate() },
		},
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("build-pod webhook listening", "addr", s.opts.Addr)
		errCh <- srv.ListenAndServeTLS("", "")
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("webhook: %w", err)
	}
}

// certificate returns the serving certificate, re-reading the files at most
// once a minute so a renewed Secret takes effect without a restart.
func (s *Server) certificate() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cert != nil && time.Since(s.loadedAt) < time.Minute {
		return s.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(s.opts.TLSDir, "tls.crt"), filepath.Join(s.opts.TLSDir, "tls.key"))
	if err != nil {
		if s.cert != nil {
			return s.cert, nil
		}
		return nil, err
	}
	s.cert, s.loadedAt = &cert, time.Now()
	return s.cert, nil
}

func (s *Server) serveBuildPod(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "not an AdmissionReview", http.StatusBadRequest)
		return
	}
	resp := &admissionv1.AdmissionResponse{UID: review.Request.UID, Allowed: true}
	var pod corev1.Pod
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err != nil {
		resp.Result = &metav1.Status{Message: "decode pod: " + err.Error(), Code: http.StatusBadRequest}
		resp.Allowed = false
	} else if patch := Patch(&pod, s.opts.Bundle); len(patch) > 0 {
		raw, _ := json.Marshal(patch)
		pt := admissionv1.PatchTypeJSONPatch
		resp.Patch, resp.PatchType = raw, &pt
		s.log.Debug("build pod patched with the trust bundle", "namespace", review.Request.Namespace, "pod", pod.Name, "containers", len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	}
	out, _ := json.Marshal(&admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Response: resp,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// Operation is one JSON patch operation.
type Operation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Patch returns the JSON patch that mounts the bundle ConfigMap into every
// init and regular container of pod and points SSL_CERT_FILE at it. Pods
// that already carry the volume are left alone; containers that already set
// the variable keep their value.
func Patch(pod *corev1.Pod, bundle string) []Operation {
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeName {
			return nil
		}
	}
	var ops []Operation
	volume := corev1.Volume{Name: VolumeName, VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: bundle}},
	}}
	if len(pod.Spec.Volumes) == 0 {
		ops = append(ops, Operation{Op: "add", Path: "/spec/volumes", Value: []corev1.Volume{volume}})
	} else {
		ops = append(ops, Operation{Op: "add", Path: "/spec/volumes/-", Value: volume})
	}
	mount := corev1.VolumeMount{Name: VolumeName, MountPath: MountPath, ReadOnly: true}
	env := corev1.EnvVar{Name: EnvVar, Value: MountPath + "/" + BundleKey}
	for i, c := range pod.Spec.InitContainers {
		ops = append(ops, containerOps(fmt.Sprintf("/spec/initContainers/%d", i), c, mount, env)...)
	}
	for i, c := range pod.Spec.Containers {
		ops = append(ops, containerOps(fmt.Sprintf("/spec/containers/%d", i), c, mount, env)...)
	}
	return ops
}

func containerOps(base string, c corev1.Container, mount corev1.VolumeMount, env corev1.EnvVar) []Operation {
	var ops []Operation
	if len(c.VolumeMounts) == 0 {
		ops = append(ops, Operation{Op: "add", Path: base + "/volumeMounts", Value: []corev1.VolumeMount{mount}})
	} else {
		ops = append(ops, Operation{Op: "add", Path: base + "/volumeMounts/-", Value: mount})
	}
	for _, e := range c.Env {
		if e.Name == env.Name {
			return ops
		}
	}
	if len(c.Env) == 0 {
		ops = append(ops, Operation{Op: "add", Path: base + "/env", Value: []corev1.EnvVar{env}})
	} else {
		ops = append(ops, Operation{Op: "add", Path: base + "/env/-", Value: env})
	}
	return ops
}
