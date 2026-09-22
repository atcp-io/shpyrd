package install

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// renderKustomize builds the kustomization at dir (a path inside tree) with
// an in-process Kustomize and returns the resulting objects. Variables are
// substituted in the source files before Kustomize parses them, so quoting
// in the manifests is preserved ("${SHPYRD_HTTPS_PORT}" stays a string).
func renderKustomize(tree fs.FS, dir string, vars map[string]string) ([]*unstructured.Unstructured, error) {
	memFS, err := treeToMemFS(tree, vars)
	if err != nil {
		return nil, err
	}
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(memFS, "/"+strings.TrimPrefix(dir, "/"))
	if err != nil {
		return nil, fmt.Errorf("kustomize %s: %w", dir, err)
	}
	raw, err := rm.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("kustomize %s: %w", dir, err)
	}
	return decodeObjects(raw)
}

// treeToMemFS copies the whole manifest tree into an in-memory file system so
// overlays can reference bases with relative paths (../../components/x/base),
// substituting variables in YAML files on the way.
func treeToMemFS(tree fs.FS, vars map[string]string) (filesys.FileSystem, error) {
	memFS := filesys.MakeFsInMemory()
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return memFS.MkdirAll("/" + p)
		}
		b, err := fs.ReadFile(tree, p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
			if b, err = Substitute(b, vars); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		return memFS.WriteFile("/"+p, b)
	})
	if err != nil {
		return nil, fmt.Errorf("load manifests: %w", err)
	}
	return memFS, nil
}

// decodeObjects splits a multi-document YAML stream into objects, flattening
// v1 List kinds and skipping empty documents.
func decodeObjects(raw []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read yaml: %w", err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal(doc, &m); err != nil {
			return nil, fmt.Errorf("decode yaml: %w", err)
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.GetKind() == "List" && strings.HasPrefix(u.GetAPIVersion(), "v1") {
			items, _, _ := unstructured.NestedSlice(m, "items")
			for _, it := range items {
				if im, ok := it.(map[string]interface{}); ok {
					out = append(out, &unstructured.Unstructured{Object: im})
				}
			}
			continue
		}
		if u.GetKind() == "" || u.GetAPIVersion() == "" {
			return nil, fmt.Errorf("object without apiVersion/kind: %s", truncate(string(doc), 120))
		}
		out = append(out, u)
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
