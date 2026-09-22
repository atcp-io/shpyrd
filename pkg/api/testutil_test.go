package api

import "k8s.io/apimachinery/pkg/runtime"

type runtimeObject = runtime.Object

func toRuntime(objs []runtimeObject) []runtime.Object {
	out := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o)
	}
	return out
}
