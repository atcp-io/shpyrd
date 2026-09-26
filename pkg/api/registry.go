package api

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shpyrd-io/shpyrd/internal/controller"
	"github.com/shpyrd-io/shpyrd/pkg/install"
	"github.com/shpyrd-io/shpyrd/pkg/registry"
)

// RegistryGC is the controller's garbage collector, shared with the API so
// the dashboard can start a collection and show its state.
type RegistryGC interface {
	Trigger(ctx context.Context) error
	Status(ctx context.Context) controller.GCStatus
}

// RegistryInfo feeds the cluster page's Registry card (RFC-0059).
type RegistryInfo struct {
	// Mode is "in-cluster" or "external".
	Mode string `json:"mode"`
	Host string `json:"host"`
	// TLS says the in-cluster registry serves TLS from the platform CA.
	TLS   bool `json:"tls"`
	Ready bool `json:"ready"`
	// Message explains a not-ready registry and its consequence.
	Message string           `json:"message,omitempty"`
	Storage *RegistryStorage `json:"storage,omitempty"`
	Images  *RegistryImages  `json:"images,omitempty"`
	// Certificate of the in-cluster registry.
	Certificate *RegistryCertificate `json:"certificate,omitempty"`
	GC          *controller.GCStatus `json:"gc,omitempty"`
}

// RegistryStorage is the volume usage of the in-cluster registry.
type RegistryStorage struct {
	UsedBytes     float64 `json:"usedBytes"`
	CapacityBytes float64 `json:"capacityBytes"`
	Size          string  `json:"size"` // the requested size (SHPYRD_REGISTRY_SIZE)
}

// RegistryImages summarises the catalog.
type RegistryImages struct {
	Repositories int                  `json:"repositories"`
	Tags         int                  `json:"tags"`
	Largest      []RegistryRepository `json:"largest"` // by tag count, at most eight
	Error        string               `json:"error,omitempty"`
}

// RegistryRepository is one repository with its tag count.
type RegistryRepository struct {
	Name string `json:"name"`
	Tags int    `json:"tags"`
}

// RegistryCertificate is the serving certificate's lifetime.
type RegistryCertificate struct {
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"notAfter"`
}

// registryCache avoids hitting the registry catalog on every dashboard poll.
type registryCache struct {
	mu     sync.Mutex
	at     time.Time
	images *RegistryImages
}

func (s *Server) registryInfo(c *gin.Context) {
	ctx := c.Request.Context()
	host, _, _ := registry.SplitReference(s.vars("SHPYRD_REGISTRY_HOST") + "/x")
	inCluster := s.vars("SHPYRD_REGISTRY_IP") != ""
	out := RegistryInfo{Mode: "external", Host: s.vars("SHPYRD_REGISTRY_HOST"), Ready: true}
	if inCluster {
		out.Mode, out.TLS = "in-cluster", true
		out.Ready, out.Message = s.registryReady(ctx)
		out.Storage = s.registryStorage(ctx)
		out.Certificate = s.registryCertificate(ctx)
		if s.opts.RegistryGC != nil {
			st := s.opts.RegistryGC.Status(ctx)
			out.GC = &st
		}
	}
	out.Images = s.registryImages(ctx, host)
	c.JSON(http.StatusOK, out)
}

func (s *Server) registryGC(c *gin.Context) {
	if s.opts.RegistryGC == nil || s.vars("SHPYRD_REGISTRY_IP") == "" {
		abort(c, http.StatusNotImplemented, errors.New("garbage collection applies to the in-cluster registry only"))
		return
	}
	if err := s.opts.RegistryGC.Trigger(c.Request.Context()); err != nil {
		abort(c, http.StatusConflict, err)
		return
	}
	s.audit(c, "", "registry.gc", "registry", "garbage collection started")
	c.JSON(http.StatusAccepted, gin.H{"status": "started"})
}

func (s *Server) registryReady(ctx context.Context) (bool, string) {
	dep, err := s.kube.Kube.AppsV1().Deployments(s.kube.Namespace).Get(ctx, "registry", metav1.GetOptions{})
	if err != nil {
		return false, "registry deployment not found: builds and new instances on nodes without the image wait"
	}
	if dep.Status.ReadyReplicas == 0 {
		return false, "registry is not ready: builds and new instances on nodes without the image wait"
	}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "config" && v.ConfigMap != nil && v.ConfigMap.Name == "registry-config-readonly" {
			return true, "read-only while garbage collection runs: pulls work, builds wait"
		}
	}
	return true, ""
}

func (s *Server) registryStorage(ctx context.Context) *RegistryStorage {
	st := &RegistryStorage{Size: s.vars(install.VarRegistrySize)}
	if s.prom == nil {
		return st
	}
	sel := `{namespace="` + s.kube.Namespace + `",persistentvolumeclaim="registry-data"}`
	if smp, err := s.prom.Query(ctx, "kubelet_volume_stats_used_bytes"+sel); err == nil && len(smp) > 0 {
		st.UsedBytes = smp[0].Value
	}
	if smp, err := s.prom.Query(ctx, "kubelet_volume_stats_capacity_bytes"+sel); err == nil && len(smp) > 0 {
		st.CapacityBytes = smp[0].Value
	}
	return st
}

func (s *Server) registryCertificate(ctx context.Context) *RegistryCertificate {
	sec, err := s.kube.Kube.CoreV1().Secrets(s.kube.Namespace).Get(ctx, "registry-tls", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(sec.Data[corev1.TLSCertKey])
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return &RegistryCertificate{Issuer: cert.Issuer.CommonName, NotAfter: cert.NotAfter}
}

// registryImages reads the catalog with the platform credential, cached for
// a minute. Provider registries without a catalog API report an error the
// card shows as "not available".
func (s *Server) registryImages(ctx context.Context, host string) *RegistryImages {
	s.regCache.mu.Lock()
	defer s.regCache.mu.Unlock()
	if s.regCache.images != nil && time.Since(s.regCache.at) < time.Minute {
		return s.regCache.images
	}
	out := &RegistryImages{}
	var data []byte
	if name := s.vars(install.VarRegistrySecret); name != "" {
		if sec, err := s.kube.Kube.CoreV1().Secrets(s.kube.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			data = sec.Data[corev1.DockerConfigJsonKey]
		}
	}
	cl := registry.FromDockerConfig(host, data)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	repos, err := cl.Catalog(cctx, 500)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Repositories = len(repos)
		for _, r := range repos {
			tags, err := cl.Tags(cctx, r)
			if err != nil {
				out.Error = err.Error()
				break
			}
			out.Tags += len(tags)
			out.Largest = append(out.Largest, RegistryRepository{Name: r, Tags: len(tags)})
		}
		sort.Slice(out.Largest, func(i, j int) bool { return out.Largest[i].Tags > out.Largest[j].Tags })
		if len(out.Largest) > 8 {
			out.Largest = out.Largest[:8]
		}
	}
	s.regCache.images, s.regCache.at = out, time.Now()
	return out
}

// vars reads an install variable from the configured lookup.
func (s *Server) vars(name string) string {
	if s.opts.Vars != nil {
		return s.opts.Vars(name)
	}
	return ""
}
