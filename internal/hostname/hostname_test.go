package hostname

import (
	"errors"
	"strings"
	"testing"
)

func TestLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "already safe", in: "postgres", want: "postgres"},
		{name: "uppercase is lowered", in: "MyApp", want: "myapp"},
		{name: "spaces and underscores become one hyphen", in: "My  App_DB", want: "my-app-db"},
		{name: "dots never create labels", in: "api.v2", want: "api-v2"},
		{name: "leading and trailing symbols are trimmed", in: "--shop!!", want: "shop"},
		{name: "non-ASCII letters become hyphens", in: "café prod", want: "caf-prod"},
		{name: "digits are kept", in: "Redis 7", want: "redis-7"},
		{name: "nothing usable falls back to a hash", in: "日本", want: "x-" + shortHash("日本", 8)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Label(tt.in)
			if got != tt.want {
				t.Errorf("Label(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !validLabel(got) {
				t.Errorf("Label(%q) = %q is not a valid DNS label", tt.in, got)
			}
		})
	}
}

func TestLabel_LongNamesAreCutDeterministicallyAndStayDistinct(t *testing.T) {
	a := strings.Repeat("a", 70) + "-one"
	b := strings.Repeat("a", 70) + "-two"
	la, lb := Label(a), Label(b)
	for _, l := range []string{la, lb} {
		if len(l) > 63 || !validLabel(l) {
			t.Errorf("label %q (len %d) is not a valid DNS label", l, len(l))
		}
	}
	if la == lb {
		t.Errorf("Label cut two different long names to the same label %q", la)
	}
	if Label(a) != la {
		t.Errorf("Label is not deterministic for %q", a)
	}
}

func TestNames_Hostname(t *testing.T) {
	tests := []struct {
		name  string
		names Names
		want  string
	}{
		{
			name:  "application or database",
			names: Names{Context: "prod", AppName: "acme-postgres-a1b2c3"},
			want:  "acme-postgres-a1b2c3.internal",
		},
		{
			name:  "service inside a compose stack",
			names: Names{Context: "prod", AppName: "acme-billing-x1y2z3", ComposeService: "postgres"},
			want:  "postgres.acme-billing-x1y2z3.internal",
		},
		{
			name:  "dots and underscores of an appName become hyphens",
			names: Names{Context: "prod", AppName: "Acme_Billing.x1y2z3", ComposeService: "Worker_1"},
			want:  "worker-1.acme-billing-x1y2z3.internal",
		},
		{
			name:  "a Dokploy ID standing in for an unknown appName",
			names: Names{Context: "prod", AppName: "pg_Main-7Xq"},
			want:  "pg-main-7xq.internal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.names.Hostname()
			if err != nil {
				t.Fatalf("Hostname: %v", err)
			}
			if got != tt.want {
				t.Errorf("Hostname = %q, want %q", got, tt.want)
			}
			if !Valid(got) {
				t.Errorf("Hostname %q is not Valid", got)
			}
		})
	}
}

func TestNames_HostnameMissingName(t *testing.T) {
	for _, n := range []Names{
		{AppName: "acme-postgres-a1b2c3"},
		{Context: "prod", ComposeService: "postgres"},
	} {
		if _, err := n.Hostname(); !errors.Is(err, ErrIncomplete) {
			t.Errorf("Hostname(%+v) error = %v, want ErrIncomplete", n, err)
		}
	}
}

func TestNames_HostnameNeverExceeds253(t *testing.T) {
	long := strings.Repeat("x", 63)
	n := Names{Context: long + "c", AppName: long + "a", ComposeService: long + "s"}
	for _, withContext := range []bool{false, true} {
		got, err := n.hostname(withContext, shortHash("id", 16))
		if err != nil {
			t.Fatalf("hostname: %v", err)
		}
		if len(got) > 253 || !Valid(got) {
			t.Errorf("hostname = %q (len %d), want a valid name of at most 253 bytes", got, len(got))
		}
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"db.shop.acme.prod.internal", true},
		{"internal", false},
		{"db.shop.acme.prod.local", false},
		{"Db.shop.acme.prod.internal", false},
		{"-db.shop.internal", false},
		{"db-.shop.internal", false},
		{"db..shop.internal", false},
		{"db_x.shop.internal", false},
		{strings.Repeat("a", 64) + ".internal", false},
		{strings.Repeat(strings.Repeat("a", 60)+".", 5) + "internal", false},
	}
	for _, tt := range tests {
		if got := Valid(tt.host); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestReserved(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"host.docker.internal", true},
		{"docker.internal", true},
		{"metadata.google.internal", true},
		{"host.containers.internal", true},
		{"acme-docker.internal", false},
		{"postgres.acme-billing-x1y2z3.internal", false},
	}
	for _, tt := range tests {
		if got := Reserved(tt.host); got != tt.want {
			t.Errorf("Reserved(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestAssign_DistinctNamesStayPlain(t *testing.T) {
	targets := []Target{
		{ID: "a", Names: Names{Context: "prod", AppName: "acme-postgres-a1b2c3"}},
		{ID: "b", Names: Names{Context: "prod", AppName: "acme-billing-x1y2z3", ComposeService: "postgres"}},
		{ID: "c", Names: Names{Context: "prod", AppName: "acme-billing-x1y2z3", ComposeService: "redis"}},
	}
	got, err := Assign(targets)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	want := map[string]string{
		"a": "acme-postgres-a1b2c3.internal",
		"b": "postgres.acme-billing-x1y2z3.internal",
		"c": "redis.acme-billing-x1y2z3.internal",
	}
	for id, host := range want {
		if got[id] != host {
			t.Errorf("Assign[%q] = %q, want %q", id, got[id], host)
		}
	}
}

func TestAssign_CollisionGetsTheContextLabel(t *testing.T) {
	// Two panels (contexts) can both hold an app named acme-billing-x1y2z3.
	first := Target{ID: "panel-a/cmp_1/postgres", Names: Names{Context: "acme-prod", AppName: "acme-billing-x1y2z3", ComposeService: "postgres"}}
	second := Target{ID: "panel-b/cmp_2/postgres", Names: Names{Context: "acme-staging", AppName: "acme-billing-x1y2z3", ComposeService: "postgres"}}

	got, err := Assign([]Target{first, second})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if want := "postgres.acme-billing-x1y2z3.internal"; got[first.ID] != want {
		t.Errorf("oldest target = %q, want the plain name %q", got[first.ID], want)
	}
	if want := "postgres.acme-billing-x1y2z3.acme-staging.internal"; got[second.ID] != want {
		t.Errorf("newer target = %q, want %q", got[second.ID], want)
	}

	// Deterministic: the same input order always yields the same names.
	again, err := Assign([]Target{first, second})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	for id := range got {
		if again[id] != got[id] {
			t.Errorf("Assign is not deterministic for %q: %q then %q", id, got[id], again[id])
		}
	}
}

func TestAssign_CollisionInOneContextFallsBackToTheHash(t *testing.T) {
	// "Acme_App" and "acme.app" normalize to the same label within one
	// context, so the context label cannot tell them apart.
	first := Target{ID: "svc-1", Names: Names{Context: "prod", AppName: "Acme_App"}}
	second := Target{ID: "svc-2", Names: Names{Context: "prod", AppName: "acme.app"}}
	third := Target{ID: "svc-3", Names: Names{Context: "prod", AppName: "ACME-APP"}}

	got, err := Assign([]Target{first, second, third})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	want := map[string]string{
		"svc-1": "acme-app.internal",
		"svc-2": "acme-app.prod.internal",
		"svc-3": "acme-app-" + shortHash("svc-3", 6) + ".prod.internal",
	}
	for id, host := range want {
		if got[id] != host {
			t.Errorf("Assign[%q] = %q, want %q", id, got[id], host)
		}
	}
}

func TestAssign_NeverClaimsReservedNames(t *testing.T) {
	got, err := Assign([]Target{{ID: "a", Names: Names{Context: "prod", AppName: "docker", ComposeService: "host"}}})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if want := "host.docker.prod.internal"; got["a"] != want {
		t.Errorf("Assign = %q, want %q instead of host.docker.internal", got["a"], want)
	}
}

func TestAssign_SuffixNeverStealsAnotherPlainName(t *testing.T) {
	// A compose service literally named like the disambiguated form keeps
	// its plain name, and the colliding target gets a longer suffix instead.
	plain := Target{ID: "svc-1", Names: Names{Context: "c", AppName: "db"}}
	dup := Target{ID: "svc-2", Names: Names{Context: "c", AppName: "DB"}}
	dup2 := Target{ID: "svc-3", Names: Names{Context: "c", AppName: "Db"}}
	squatter := Target{ID: "svc-4", Names: Names{Context: "x", AppName: "c", ComposeService: "db-" + shortHash("svc-3", 6)}}

	got, err := Assign([]Target{plain, dup, dup2, squatter})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	seen := map[string]string{}
	for id, host := range got {
		if other, ok := seen[host]; ok {
			t.Fatalf("%q and %q both map to %q", id, other, host)
		}
		seen[host] = id
	}
	want := map[string]string{
		"svc-1": "db.internal",
		"svc-2": "db.c.internal",
		"svc-3": "db-" + shortHash("svc-3", 8) + ".c.internal",
		"svc-4": "db-" + shortHash("svc-3", 6) + ".c.internal",
	}
	for id, host := range want {
		if got[id] != host {
			t.Errorf("Assign[%q] = %q, want %q", id, got[id], host)
		}
	}
}

func TestAssign_RejectsDuplicateIDsAndIncompleteNames(t *testing.T) {
	n := Names{Context: "c", AppName: "a"}
	if _, err := Assign([]Target{{ID: "a", Names: n}, {ID: "a", Names: n}}); err == nil {
		t.Error("Assign accepted a duplicate ID")
	}
	if _, err := Assign([]Target{{ID: "a", Names: Names{ComposeService: "s"}}}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("Assign error = %v, want ErrIncomplete", err)
	}
}
