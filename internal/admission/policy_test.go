package admission_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"sigs.k8s.io/yaml"
)

type validation struct {
	Expression string `json:"expression"`
	Message    string `json:"message"`
}

type matchCondition struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

type policy struct {
	Spec struct {
		MatchConditions []matchCondition `json:"matchConditions"`
		Validations     []validation     `json:"validations"`
	} `json:"spec"`
}

func loadPolicy(t *testing.T) policy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "admission-policy.yaml"))
	if err != nil {
		t.Fatalf("read policy: %v", err)
	}
	first := strings.SplitN(string(data), "\n---\n", 2)[0]
	var p policy
	if uerr := yaml.Unmarshal([]byte(first), &p); uerr != nil {
		t.Fatalf("unmarshal policy: %v", uerr)
	}
	if len(p.Spec.Validations) == 0 {
		t.Fatal("policy loaded with zero validations")
	}
	return p
}

func newEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		ext.Strings(),
		cel.Variable("object", cel.DynType),
	)
	if err != nil {
		t.Fatalf("new env: %v", err)
	}
	return env
}

func evalExpr(t *testing.T, env *cel.Env, expr string, obj any) bool {
	t.Helper()
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("compile %q: %v", expr, iss.Err())
	}
	prog, err := env.Program(ast)
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	out, _, err := prog.Eval(map[string]any{"object": obj})
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		t.Fatalf("eval returned non-bool %T for %q", out.Value(), expr)
	}
	return b
}

func objWith(anns map[string]string) map[string]any {
	m := make(map[string]any, len(anns))
	for k, v := range anns {
		m[k] = v
	}
	return map[string]any{
		"metadata": map[string]any{
			"annotations": m,
		},
	}
}

func TestPolicy_AcceptsValidAnnotations(t *testing.T) {
	p := loadPolicy(t)
	env := newEnv(t)

	cases := []struct {
		name string
		anns map[string]string
	}{
		{"dns_true", map[string]string{"greydns.io/dns": "true"}},
		{"dns_false", map[string]string{"greydns.io/dns": "false"}},
		{"ttl_1", map[string]string{"greydns.io/dns": "true", "greydns.io/ttl": "1"}},
		{"ttl_300", map[string]string{"greydns.io/dns": "true", "greydns.io/ttl": "300"}},
		{"record_type_A", map[string]string{"greydns.io/dns": "true", "greydns.io/record-type": "A"}},
		{"record_type_AAAA", map[string]string{"greydns.io/dns": "true", "greydns.io/record-type": "AAAA"}},
		{"record_type_CNAME", map[string]string{"greydns.io/dns": "true", "greydns.io/record-type": "CNAME"}},
		{"zone_basic", map[string]string{"greydns.io/dns": "true", "greydns.io/zone": "example.com"}},
		{"zone_trailing_dot", map[string]string{"greydns.io/dns": "true", "greydns.io/zone": "example.com."}},
		{"zone_subdomain", map[string]string{"greydns.io/dns": "true", "greydns.io/zone": "team.example.com"}},
		{"domain_single", map[string]string{"greydns.io/dns": "true", "greydns.io/domain": "api.example.com"}},
		{"domain_multi", map[string]string{"greydns.io/dns": "true", "greydns.io/domain": "a.example.com,b.example.com"}},
		{"domain_padded_whitespace", map[string]string{
			"greydns.io/dns":    "true",
			"greydns.io/domain": "  a.example.com , b.example.com  ",
		}},
		{"domain_empty_items_tolerated", map[string]string{
			"greydns.io/dns":    "true",
			"greydns.io/domain": "a.example.com,,b.example.com",
		}},
		{"full_config", map[string]string{
			"greydns.io/dns":         "true",
			"greydns.io/zone":        "example.com",
			"greydns.io/domain":      "api.example.com,api-v2.example.com",
			"greydns.io/ttl":         "300",
			"greydns.io/record-type": "AAAA",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := objWith(tc.anns)
			for _, v := range p.Spec.Validations {
				if !evalExpr(t, env, v.Expression, obj) {
					t.Fatalf("unexpected rejection: %s", v.Message)
				}
			}
		})
	}
}

func TestPolicy_RejectsInvalidAnnotations(t *testing.T) {
	p := loadPolicy(t)
	env := newEnv(t)

	cases := []struct {
		name    string
		anns    map[string]string
		wantMsg string
	}{
		{"dns_yes", map[string]string{"greydns.io/dns": "yes"}, "greydns.io/dns"},
		{"dns_1", map[string]string{"greydns.io/dns": "1"}, "greydns.io/dns"},
		{"dns_True_wrong_case", map[string]string{"greydns.io/dns": "True"}, "greydns.io/dns"},
		{"ttl_zero", map[string]string{"greydns.io/ttl": "0"}, "greydns.io/ttl"},
		{"ttl_leading_zero", map[string]string{"greydns.io/ttl": "01"}, "greydns.io/ttl"},
		{"ttl_negative", map[string]string{"greydns.io/ttl": "-5"}, "greydns.io/ttl"},
		{"ttl_float", map[string]string{"greydns.io/ttl": "1.5"}, "greydns.io/ttl"},
		{"ttl_nonnumeric", map[string]string{"greydns.io/ttl": "abc"}, "greydns.io/ttl"},
		{"record_type_MX", map[string]string{"greydns.io/record-type": "MX"}, "greydns.io/record-type"},
		{"record_type_lowercase", map[string]string{"greydns.io/record-type": "a"}, "greydns.io/record-type"},
		{"zone_empty", map[string]string{"greydns.io/zone": ""}, "greydns.io/zone"},
		{"zone_single_label", map[string]string{"greydns.io/zone": "localhost"}, "greydns.io/zone"},
		{"zone_with_space", map[string]string{"greydns.io/zone": "ex ample.com"}, "greydns.io/zone"},
		{"domain_invalid_in_list", map[string]string{"greydns.io/domain": "api.example.com,not valid"}, "greydns.io/domain"},
		{"domain_single_label", map[string]string{"greydns.io/domain": "localhost"}, "greydns.io/domain"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := objWith(tc.anns)
			for _, v := range p.Spec.Validations {
				if !evalExpr(t, env, v.Expression, obj) {
					if !strings.Contains(v.Message, tc.wantMsg) {
						t.Fatalf("rejected by %q, expected message mentioning %q", v.Message, tc.wantMsg)
					}
					return
				}
			}
			t.Fatalf("expected some validation mentioning %q to reject, all passed", tc.wantMsg)
		})
	}
}

func TestPolicy_MatchConditionSkipsNonGreydnsObjects(t *testing.T) {
	p := loadPolicy(t)
	env := newEnv(t)

	var mc matchCondition
	for _, c := range p.Spec.MatchConditions {
		if c.Name == "has-greydns-annotation" {
			mc = c
			break
		}
	}
	if mc.Expression == "" {
		t.Fatal("has-greydns-annotation match condition not found")
	}

	cases := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"with_greydns", objWith(map[string]string{"greydns.io/dns": "true"}), true},
		{"only_unrelated", objWith(map[string]string{"team": "payments"}), false},
		{"empty_annotations", objWith(map[string]string{}), false},
		{"no_annotations_key", map[string]any{"metadata": map[string]any{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalExpr(t, env, mc.Expression, tc.obj); got != tc.want {
				t.Fatalf("match condition got %v, want %v", got, tc.want)
			}
		})
	}
}
