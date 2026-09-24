package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"shpyrd/pkg/api"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
)

// shpyrd cluster registry [gc] (RFC-0059): the registry card as text.
func newClusterRegistryCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Show the image registry: mode, health, storage, images, garbage collection",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			raw, err := serverRequest(ctx, k, "GET", "api/cluster/registry", nil, "")
			if err != nil {
				return err
			}
			var info api.RegistryInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				return fmt.Errorf("unexpected response: %s", truncate(string(raw), 200))
			}
			printRegistry(cmd.OutOrStdout(), &info)
			return nil
		},
	}
	var wait bool
	gc := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim the space of deleted images now (the registry is read-only for a few minutes: builds wait, pulls work)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := signalContext()
			out := cmd.OutOrStdout()
			k, err := kube.Connect(kube.Options{Kubeconfig: g.kubeconfig, Context: g.kubeCtx})
			if err != nil {
				return err
			}
			if _, err := serverRequest(ctx, k, "POST", "api/cluster/registry/gc", nil, ""); err != nil {
				return err
			}
			fmt.Fprintln(out, "Garbage collection started (the registry is read-only until it finishes).")
			if !wait {
				fmt.Fprintln(out, "Follow it with `shpyrd cluster registry`.")
				return nil
			}
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
				raw, err := serverRequest(ctx, k, "GET", "api/cluster/registry", nil, "")
				if err != nil {
					return err
				}
				var info api.RegistryInfo
				if err := json.Unmarshal(raw, &info); err != nil {
					return err
				}
				if info.GC != nil && !info.GC.Running {
					if info.GC.LastResult != "ok" {
						return fmt.Errorf("garbage collection failed: %s", info.GC.LastResult)
					}
					fmt.Fprintf(out, "Done in %s: reclaimed %s, %s in use.\n", info.GC.LastDuration, humanBytes(int(info.GC.ReclaimedBytes)), humanBytes(int(info.GC.UsedBytes)))
					return nil
				}
				fmt.Fprint(out, ".")
			}
		},
	}
	gc.Flags().BoolVar(&wait, "wait", true, "wait for the collection to finish")
	cmd.AddCommand(gc)
	return cmd
}

func printRegistry(out io.Writer, info *api.RegistryInfo) {
	switch info.Mode {
	case "in-cluster":
		fmt.Fprintf(out, "Registry:     in-cluster at %s (TLS from the platform CA)\n", info.Host)
	default:
		fmt.Fprintf(out, "Registry:     %s (external)\n", info.Host)
	}
	state := "ready"
	if !info.Ready {
		state = "not ready"
	}
	if info.Message != "" {
		state += " - " + info.Message
	}
	fmt.Fprintf(out, "Health:       %s\n", state)
	if st := info.Storage; st != nil {
		if st.CapacityBytes > 0 {
			fmt.Fprintf(out, "Storage:      %s of %s used (%.0f%%)", humanBytes(int(st.UsedBytes)), humanBytes(int(st.CapacityBytes)), 100*st.UsedBytes/st.CapacityBytes)
			if st.UsedBytes/st.CapacityBytes >= 0.8 {
				fmt.Fprintf(out, "  grow it: shpyrd cluster init --set SHPYRD_REGISTRY_SIZE=<size>")
			}
			fmt.Fprintln(out)
		} else {
			fmt.Fprintf(out, "Storage:      %s requested (usage appears where the storage class reports volume metrics, as cloud block volumes do)\n", st.Size)
		}
	}
	if im := info.Images; im != nil {
		if im.Error != "" && im.Repositories == 0 {
			fmt.Fprintf(out, "Images:       not available (%s)\n", truncate(im.Error, 80))
		} else {
			fmt.Fprintf(out, "Images:       %d repositories, %d tags\n", im.Repositories, im.Tags)
			for _, r := range im.Largest {
				fmt.Fprintf(out, "              %-40s %d\n", r.Name, r.Tags)
			}
		}
	}
	if gc := info.GC; gc != nil {
		line := "off"
		if gc.Schedule != "" {
			line = fmt.Sprintf("schedule %q (UTC)", gc.Schedule)
			if gc.NextRun != nil {
				line += ", next " + gc.NextRun.Local().Format("Mon 02 Jan 15:04")
			}
		}
		if gc.Running {
			line += "; running since " + gc.StartedAt.Local().Format("15:04:05")
		} else if gc.LastRun != nil {
			line += fmt.Sprintf("; last %s: %s", ago(*gc.LastRun), gc.LastResult)
			if gc.LastResult == "ok" {
				line += fmt.Sprintf(", reclaimed %s in %s", humanBytes(int(gc.ReclaimedBytes)), gc.LastDuration)
			}
		} else {
			line += "; never run"
		}
		fmt.Fprintf(out, "Collection:   %s\n", line)
	}
	if c := info.Certificate; c != nil {
		fmt.Fprintf(out, "Certificate:  from %s, expires %s (renews automatically)\n", c.Issuer, c.NotAfter.Local().Format("2006-01-02"))
	}
}

func ago(t time.Time) string {
	d := time.Since(t).Truncate(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// serverRequest calls the shpyrd API through the Kubernetes API server
// proxy (works with any kubeconfig and no ingress), authenticating with the
// admin token from the cluster in X-Shpyrd-Token (the proxy strips
// Authorization headers).
func serverRequest(ctx context.Context, k *kube.Client, method, path string, body []byte, contentType string) ([]byte, error) {
	rc := k.Kube.CoreV1().RESTClient()
	var req = rc.Verb(method).
		Namespace(install.DefaultSystemNamespace).
		Resource("services").
		Name("shpyrd-server:http").
		SubResource("proxy").
		Suffix(strings.TrimPrefix(path, "/"))
	if body != nil {
		req = req.Body(body)
		if contentType != "" {
			req = req.SetHeader("Content-Type", contentType)
		}
	}
	if sec, err := k.Kube.CoreV1().Secrets(install.DefaultSystemNamespace).Get(ctx, install.AdminTokenSecretName, metav1.GetOptions{}); err == nil {
		if tok := strings.TrimSpace(string(sec.Data["token"])); tok != "" {
			req = req.SetHeader("X-Shpyrd-Token", tok)
		}
	}
	raw, err := req.Do(ctx).Raw()
	if err != nil {
		msg := strings.TrimSpace(string(raw))
		if msg != "" {
			return nil, fmt.Errorf("shpyrd-server: %s", truncate(msg, 300))
		}
		return nil, fmt.Errorf("shpyrd-server: %w (is the base stack installed? `shpyrd cluster status`)", err)
	}
	return raw, nil
}
