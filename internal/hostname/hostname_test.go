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
			name:  "Dokploy service",
			names: Names{Context: "prod", Organization: "Acme", Project: "shop", Service: "main-db"},
			want:  "main-db.shop.acme.prod.internal",
		},
		{
			name:  "service inside a compose stack",
			names: Names{Context: "prod", Organization: "Acme", Project: "shop", Compose: "myapp", Service: "postgres"},
			want:  "postgres.myapp.shop.acme.prod.internal",
		},
		{
			name:  "display names are sanitized",
			names: Names{Context: "Prod.EU", Organization: "Acme Inc.", Project: "Shop_2", Service: "Main DB"},
			want:  "main-db.shop-2.acme-inc.prod-eu.internal",
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
	full := Names{Context: "prod", Organization: "acme", Project: "shop", Service: "db"}
	for _, field := range []string{"Context", "Organization", "Project", "Service"} {
		t.Run(field, func(t *testing.T) {
			n := full
			switch field {
			case "Context":
				n.Context = ""
			case "Organization":
				n.Organization = ""
			case "Project":
				n.Project = ""
			case "Service":
				n.Service = ""
			}
			if _, err := n.Hostname(); !errors.Is(err, ErrIncomplete) {
				t.Errorf("Hostname error = %v, want ErrIncomplete", err)
			}
		})
	}
}

func TestNames_HostnameNeverExceeds253(t *testing.T) {
	long := strings.Repeat("x", 63)
	n := Names{Context: long + "c", Organization: long + "o", Project: long + "p", Compose: long + "k", Service: long + "s"}
	got, err := n.Hostname()
	if err != nil {
		t.Fatalf("Hostname: %v", err)
	}
	if len(got) > 253 || !Valid(got) {
		t.Errorf("Hostname = %q (len %d), want a valid name of at most 253 bytes", got, len(got))
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

func TestAssign_DistinctNamesStayPlain(t *testing.T) {
	targets := []Target{
		{ID: "a", Names: Names{Context: "prod", Organization: "acme", Project: "shop", Service: "db"}},
		{ID: "b", Names: Names{Context: "prod", Organization: "acme", Project: "shop", Compose: "myapp", Service: "db"}},
	}
	got, err := Assign(targets)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	want := map[string]string{
		"a": "db.shop.acme.prod.internal",
		"b": "db.myapp.shop.acme.prod.internal",
	}
	for id, host := range want {
		if got[id] != host {
			t.Errorf("Assign[%q] = %q, want %q", id, got[id], host)
		}
	}
}

func TestAssign_CollisionAfterSanitizationIsDisambiguated(t *testing.T) {
	// "Main DB" and "main_db" sanitize to the same label; the same service
	// name in two environments of one project collides the same way.
	first := Target{ID: "svc-1", Names: Names{Context: "prod", Organization: "acme", Project: "shop", Service: "Main DB"}}
	second := Target{ID: "svc-2", Names: Names{Context: "prod", Organization: "acme", Project: "shop", Service: "main_db"}}

	got, err := Assign([]Target{first, second})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got["svc-1"] != "main-db.shop.acme.prod.internal" {
		t.Errorf("first target = %q, want the plain name", got["svc-1"])
	}
	wantSecond := "main-db-" + shortHash("svc-2", 6) + ".shop.acme.prod.internal"
	if got["svc-2"] != wantSecond {
		t.Errorf("second target = %q, want %q", got["svc-2"], wantSecond)
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

func TestAssign_SuffixNeverStealsAnotherPlainName(t *testing.T) {
	// A service literally named like the disambiguated form keeps its name,
	// and the colliding target gets a longer suffix instead.
	plain := Target{ID: "svc-1", Names: Names{Context: "c", Organization: "o", Project: "p", Service: "db"}}
	dup := Target{ID: "svc-2", Names: Names{Context: "c", Organization: "o", Project: "p", Service: "DB"}}
	squatter := Target{ID: "svc-3", Names: Names{Context: "c", Organization: "o", Project: "p", Service: "db-" + shortHash("svc-2", 6)}}

	got, err := Assign([]Target{plain, dup, squatter})
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
	if got["svc-3"] != "db-"+shortHash("svc-2", 6)+".p.o.c.internal" {
		t.Errorf("squatter = %q, want its plain name", got["svc-3"])
	}
	if want := "db-" + shortHash("svc-2", 8) + ".p.o.c.internal"; got["svc-2"] != want {
		t.Errorf("colliding target = %q, want %q", got["svc-2"], want)
	}
}

func TestAssign_RejectsDuplicateIDsAndIncompleteNames(t *testing.T) {
	n := Names{Context: "c", Organization: "o", Project: "p", Service: "s"}
	if _, err := Assign([]Target{{ID: "a", Names: n}, {ID: "a", Names: n}}); err == nil {
		t.Error("Assign accepted a duplicate ID")
	}
	if _, err := Assign([]Target{{ID: "a", Names: Names{Service: "s"}}}); !errors.Is(err, ErrIncomplete) {
		t.Errorf("Assign error = %v, want ErrIncomplete", err)
	}
}
