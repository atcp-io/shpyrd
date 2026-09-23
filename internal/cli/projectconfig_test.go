package cli

import (
	"testing"

	"sigs.k8s.io/yaml"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

func TestProjectGlobalsKey(t *testing.T) {
	cases := map[string]*shpyrdv1.Globals{
		"project: x\n":                                   nil,
		"project: x\nglobals: true\n":                    nil,
		"project: x\nglobals: false\n":                   {Disabled: true},
		"project: x\nglobals:\n  exclude: [A, B]\n":      {Exclude: []string{"A", "B"}},
		"project: x\nglobals: {exclude: [OPENAI_KEY]}\n": {Exclude: []string{"OPENAI_KEY"}},
	}
	for in, want := range cases {
		var pc projectConfig
		if err := yaml.Unmarshal([]byte(in), &pc); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		app := &shpyrdv1.App{}
		if err := pc.applyTo(app); err != nil {
			t.Fatal(err)
		}
		got := app.Spec.Globals
		switch {
		case want == nil && got != nil:
			t.Errorf("%q: got %+v, want nil", in, got)
		case want != nil && (got == nil || got.Disabled != want.Disabled || len(got.Exclude) != len(want.Exclude)):
			t.Errorf("%q: got %+v, want %+v", in, got, want)
		}
	}
	var pc projectConfig
	if err := yaml.Unmarshal([]byte("project: x\nglobals: [nope]\n"), &pc); err == nil {
		t.Error("a list is not a valid globals value")
	}
}
