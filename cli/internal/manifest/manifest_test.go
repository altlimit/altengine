package manifest

// Parity with the hosted parser's tests (test/deploymanifest.test.ts): the same
// manifests are accepted and refused with the same words.

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var H = strings.Repeat("a", 64)

type M = map[string]any

func base(over M) M {
	m := M{
		"altengine": 1,
		"name":      "guestbook",
		"instances": M{
			"datastore": M{"service": "datastore"},
			"functions": M{
				"service":   "functions",
				"functions": M{"web": M{"module": M{"url": "./web.mjs", "sha256": H}, "grants": M{"datastore:${datastore}": "write"}}},
			},
		},
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func decode(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := DecodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseOK(t *testing.T, v any, sources ...map[string]string) (*Manifest, []Ref) {
	t.Helper()
	m, refs, err := Parse(decode(t, v), sources)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m, refs
}

func parseErr(t *testing.T, v any, want string, sources ...map[string]string) {
	t.Helper()
	_, _, err := Parse(decode(t, v), sources)
	if err == nil {
		t.Fatalf("Parse accepted a manifest it should refuse with %q", want)
	}
	if !regexp.MustCompile(want).MatchString(err.Error()) {
		t.Fatalf("Parse error = %q, want it to match %q", err.Error(), want)
	}
}

func inst(m M, key string) M { return m["instances"].(M)[key].(M) }

func TestParseAcceptsASmallManifestAndListsItsReferences(t *testing.T) {
	m, refs := parseOK(t, base(nil))
	if g := m.Instance("functions").Functions[0].Grants; !reflect.DeepEqual(g, map[string]string{"datastore:${datastore}": "write"}) {
		t.Fatalf("grants = %v", g)
	}
	want := []Ref{{From: "functions", Field: "functions.web.grants", Target: "datastore", Service: "datastore", Level: "write"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %+v", refs)
	}
	// Instances keep the order they were written in.
	if m.Instances[0].Key != "datastore" || m.Instances[1].Key != "functions" {
		t.Fatalf("order = %s, %s", m.Instances[0].Key, m.Instances[1].Key)
	}
}

func TestParseKeepsTheOrderInstancesAreWrittenIn(t *testing.T) {
	raw, err := DecodeJSON([]byte(`{"altengine":1,"name":"app","instances":{"jobs":{"service":"tasks","queues":{"z":{},"a":{}}},"datastore":{"service":"datastore"},"auth":{"service":"auth"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := Parse(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, d := range m.Instances {
		keys = append(keys, d.Key)
	}
	if !reflect.DeepEqual(keys, []string{"jobs", "datastore", "auth"}) || m.Instances[0].Queues[0].Name != "z" {
		t.Fatalf("order = %v, queues %+v", keys, m.Instances[0].Queues)
	}
}

func TestParseRefusesVersionNameAndEmpty(t *testing.T) {
	parseErr(t, base(M{"altengine": 2}), `format version`)
	parseErr(t, base(M{"name": "Bad Name"}), `manifest.name`)
	parseErr(t, base(M{"instances": M{}}), `at least one`)
}

func TestParseRefusesServicesALinkMustNotSetUp(t *testing.T) {
	for _, svc := range []string{"automation", "container", "mailbox"} {
		parseErr(t, base(M{"instances": M{"x": M{"service": svc}}}), `must be one of`)
	}
}

func TestParseRefusesALiteralName(t *testing.T) {
	m := base(nil)
	inst(m, "functions")["functions"].(M)["web"].(M)["grants"] = M{"datastore:orders": "write"}
	parseErr(t, m, `must be a reference like`)
}

func TestParseRefusesUndeclaredOrWrongService(t *testing.T) {
	m := base(nil)
	web := inst(m, "functions")["functions"].(M)["web"].(M)
	web["grants"] = M{"datastore:${nope}": "write"}
	parseErr(t, m, `not declared`)
	web["grants"] = M{"datastore:${functions}": "write"}
	parseErr(t, m, `is a functions instance, not datastore`)
}

func TestParseRefusesAStrayPlaceholder(t *testing.T) {
	m := base(nil)
	inst(m, "datastore")["config"] = M{"rateLimit": "${datastore}"}
	parseErr(t, m, `only allowed in reference fields`)
}

func TestParseRefusesConfigThatMovesCodeSiteOrRegion(t *testing.T) {
	fn := base(nil)
	inst(fn, "functions")["config"] = M{"functions": []any{}}
	parseErr(t, fn, `cannot be set by a manifest`)
	parseErr(t, base(M{"instances": M{"static": M{"service": "static", "config": M{"activeDeployment": "x"}}}}), `cannot be set by a manifest`)
	rg := base(nil)
	inst(rg, "datastore")["config"] = M{"region": "eu"}
	parseErr(t, rg, `set region on the instance`)
}

func TestParseRefusesAKeyNamedAfterAnotherService(t *testing.T) {
	parseErr(t, base(M{"instances": M{"auth": M{"service": "datastore"}}}), `named after the auth service`)
}

func TestParseReadsReferencesInAccessTriggersLiveAndQueues(t *testing.T) {
	_, refs := parseOK(t, M{
		"altengine": 1, "name": "app",
		"uses": M{"orders": M{"service": "datastore", "level": "read"}},
		"instances": M{
			"datastore": M{"service": "datastore", "config": M{"triggers": []any{M{"match": M{"collection": "e"}, "queue": "${jobs}/q"}}, "live": M{"channelInstance": "${live}", "collections": M{}}}},
			"live":      M{"service": "channel"},
			"jobs":      M{"service": "tasks", "queues": M{"q": M{"target": M{"function": "${functions}/web"}}}},
			"auth":      M{"service": "auth", "config": M{"access": M{"datastore:${datastore}": M{"level": "write"}, "datastore:${orders}": M{"level": "read"}}}},
			"functions": M{"service": "functions"},
		},
	})
	var got []string
	for _, r := range refs {
		got = append(got, r.From+":"+r.Target+":"+r.Level)
	}
	sort.Strings(got)
	want := []string{"auth:datastore:write", "auth:orders:read", "datastore:jobs:write", "datastore:live:write", "jobs:functions:read"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %v", got)
	}
}

func TestParseRequiresAnEgressSecretToNameHosts(t *testing.T) {
	m := base(nil)
	inst(m, "functions")["secrets"] = M{"KEY": M{"egress": true}}
	parseErr(t, m, `must name the hosts`)
}

func TestParseArchivePaths(t *testing.T) {
	m := base(nil)
	web := inst(m, "functions")["functions"].(M)["web"].(M)
	web["module"] = M{"zip": "dist/web.mjs"}
	parseErr(t, m, `manifest.source.zip is not set`)
	m["source"] = M{"zip": "https://example.com/a.zip"}
	parseOK(t, m)
	web["module"] = M{"zip": "../etc/passwd"}
	parseErr(t, m, `not a valid archive path`)
}

func TestParseChecksSiteFiles(t *testing.T) {
	site := func(files any) M {
		return base(M{"instances": M{"static": M{"service": "static", "site": M{"base": "./dist/", "files": files}}}})
	}
	parseErr(t, site(M{}), `at least one file`)
	parseErr(t, site(M{"/../x": M{"hash": H, "size": 1}}), `not a valid site path`)
	parseErr(t, site(M{"/x": M{"hash": "nope", "size": 1}}), `hex sha256`)
	m, _ := parseOK(t, site(M{"index.html": M{"hash": H, "size": 1}}))
	got := m.Instance("static").Site
	if got.Base != "./dist/" || !reflect.DeepEqual(got.Files, map[string]SiteFile{"/index.html": {Hash: H, Size: 1}}) {
		t.Fatalf("site = %+v", got)
	}
}

func TestParseRefusesNonHTTPSAbsoluteURLs(t *testing.T) {
	m := base(nil)
	inst(m, "functions")["functions"].(M)["web"].(M)["module"] = M{"url": "file:///etc/passwd", "sha256": H}
	parseErr(t, m, `https URL or a relative path`)
}

func TestNamesAndReferences(t *testing.T) {
	if n := InstanceName("guestbook", "datastore", "datastore", ""); n != "guestbook" {
		t.Fatal(n)
	}
	if n := InstanceName("guestbook", "jobs", "tasks", ""); n != "guestbook-jobs" {
		t.Fatal(n)
	}
	if ValidateAppName("my-app") != nil {
		t.Fatal("my-app refused")
	}
	for _, n := range []string{"a", "-x-", "Has Caps", strings.Repeat("x", 41), "a:b"} {
		if ValidateAppName(n) == nil {
			t.Fatalf("%q accepted", n)
		}
	}
	got, err := ResolveRefs(map[string]any{"datastore:${d}": "write", "q": "${t}/x"}, map[string]string{"d": "app", "t": "app-jobs"}, nil)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"datastore:app": "write", "q": "app-jobs/x"}) {
		t.Fatalf("resolved = %v, %v", got, err)
	}
	if _, err := ResolveRefs("${x}", map[string]string{}, nil); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("err = %v", err)
	}
}

func withVars(over M) M {
	m := M{
		"altengine": 1,
		"name":      "board-{{board}}",
		"vars":      M{"board": M{"description": "Board id", "pattern": "[a-z0-9-]{1,20}"}, "color": M{"default": "blue"}},
		"instances": M{"datastore": M{"service": "datastore", "config": M{"autoIndex": true}}},
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestVarsFillEverywhereTheAppNameIncluded(t *testing.T) {
	m, _ := parseOK(t, withVars(nil), map[string]string{"board": "acme"})
	if m.Name != "board-acme" || !reflect.DeepEqual(m.VarValues, map[string]string{"board": "acme", "color": "blue"}) {
		t.Fatalf("name %q vars %v", m.Name, m.VarValues)
	}
}

func TestVarsTakeTheFirstSourceThatHasAValue(t *testing.T) {
	if m, _ := parseOK(t, withVars(nil), map[string]string{"board": "one"}, map[string]string{"board": "two"}); m.Name != "board-one" {
		t.Fatal(m.Name)
	}
	if m, _ := parseOK(t, withVars(nil), map[string]string{}, map[string]string{"board": "two"}); m.Name != "board-two" {
		t.Fatal(m.Name)
	}
}

func TestVarsAskForARequiredValue(t *testing.T) {
	parseErr(t, withVars(nil), `enter a value for board`)
	_, _, err := Parse(decode(t, withVars(nil)), nil)
	if mv, ok := err.(*MissingVarsError); !ok || !reflect.DeepEqual(mv.Missing, []string{"board"}) {
		t.Fatalf("err = %#v", err)
	}
}

func TestVarsHoldValuesToSafeCharactersAndThePattern(t *testing.T) {
	for _, bad := range []string{"Acme", "a b", "${datastore}", "{{x}}", `q"uote`} {
		if _, _, err := Parse(decode(t, withVars(nil)), []map[string]string{{"board": bad}}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestVarsRefuseAnUndeclaredPlaceholder(t *testing.T) {
	parseErr(t, withVars(M{"title": "{{nope}}"}), `not declared in vars`, map[string]string{"board": "a"})
}

func TestEnvNamesInstancesAndUsesVars(t *testing.T) {
	m, _ := parseOK(t, M{
		"altengine": 1, "name": "app-{{team}}", "vars": M{"team": M{}},
		"instances": M{"datastore": M{"service": "datastore"}, "functions": M{"service": "functions", "env": M{"DB_NAME": "${datastore}", "TEAM": "{{team}}"}}},
	}, map[string]string{"team": "red"})
	want := []EnvVar{{Name: "DB_NAME", Value: "${datastore}"}, {Name: "TEAM", Value: "red"}}
	if !reflect.DeepEqual(m.Instance("functions").Env, want) {
		t.Fatalf("env = %+v", m.Instance("functions").Env)
	}
}

func TestEnvRefusals(t *testing.T) {
	app := func(instances M) M { return M{"altengine": 1, "name": "app", "instances": instances} }
	parseErr(t, app(M{"datastore": M{"service": "datastore", "env": M{"A": "1"}}}), `only for functions`)
	parseErr(t, app(M{"functions": M{"service": "functions", "env": M{"A": "${nope}"}}}), `not declared`)
	parseErr(t, app(M{"functions": M{"service": "functions", "env": M{"K": "1"}, "secrets": M{"K": M{}}}}), `both a secret and an env value`)
	parseErr(t, app(M{"functions": M{"service": "functions", "env": M{"DATASTORE": "x"}}}), `reserved by the runtime`)
	parseErr(t, M{"altengine": 1, "name": "app", "homepage": " javascript:alert(1)", "instances": M{"datastore": M{"service": "datastore"}}}, `https URL`)
}

func TestDutyBoardNeeds(t *testing.T) {
	app := func(instances M) M { return M{"altengine": 1, "name": "duty", "instances": instances} }
	m, _ := parseOK(t, app(M{"auth": M{"service": "auth", "suffix": "auth"}, "search": M{"service": "search", "suffix": "search"}}))
	if n := InstanceName("dutyboard", "auth", "auth", m.Instance("auth").Suffix); n != "dutyboard-auth" {
		t.Fatal(n)
	}
	parseErr(t, app(M{"jobs": M{"service": "tasks"}, "other": M{"service": "tasks", "suffix": "jobs"}}), `same name`)
	parseErr(t, app(M{"auth": M{"service": "auth", "suffix": "Bad"}}), `suffix`)

	// ${key:origin} only as an item of the two origin lists, for a hosted instance.
	parseOK(t, app(M{
		"console":   M{"service": "static"},
		"auth":      M{"service": "auth", "config": M{"settings": M{"allowedOrigins": []any{"${console:origin}", "http://localhost:5173"}}}},
		"functions": M{"service": "functions", "config": M{"corsOrigins": []any{"${console:origin}"}}},
	}))
	parseErr(t, app(M{"datastore": M{"service": "datastore"}, "functions": M{"service": "functions", "config": M{"corsOrigins": []any{"${datastore:origin}"}}}}), `has no origin`)
	parseErr(t, app(M{"console": M{"service": "static"}, "functions": M{"service": "functions", "config": M{"allowedHosts": []any{"${console:origin}"}}}}), `only allowed in reference fields`)

	// ${key:url} in env for a hosted instance.
	m, _ = parseOK(t, app(M{"console": M{"service": "static"}, "functions": M{"service": "functions", "env": M{"CONSOLE_URL": "${console:url}"}}}))
	if v := m.Instance("functions").Env[0].Value; v != "${console:url}" {
		t.Fatal(v)
	}
	parseErr(t, app(M{"datastore": M{"service": "datastore"}, "functions": M{"service": "functions", "env": M{"X": "${datastore:url}"}}}), `has no url`)

	// initialConfig is read with the same rules as config.
	m, _ = parseOK(t, app(M{"auth": M{"service": "auth", "initialConfig": M{"settings": M{"allowSignup": true}}}}))
	if !reflect.DeepEqual(m.Instance("auth").InitialConfig, map[string]any{"settings": map[string]any{"allowSignup": true}}) {
		t.Fatalf("initialConfig = %v", m.Instance("auth").InitialConfig)
	}
	parseErr(t, app(M{"static": M{"service": "static", "initialConfig": M{"activeDeployment": "x"}}}), `cannot be set`)

	// 128 indexes on one datastore, not 129.
	var indexes []any
	for i := 0; i < 128; i++ {
		indexes = append(indexes, M{"collection": "c" + string(rune('a'+i%26)), "fields": []any{"a"}})
	}
	parseOK(t, app(M{"datastore": M{"service": "datastore", "indexes": indexes}}))
	parseErr(t, app(M{"datastore": M{"service": "datastore", "indexes": append(indexes, M{"collection": "x", "fields": []any{"a"}})}}), `more than 128`)
}

func TestLimits(t *testing.T) {
	many := M{}
	for i := 0; i < 13; i++ {
		many["d"+string(rune('a'+i))] = M{"service": "datastore"}
	}
	parseErr(t, M{"altengine": 1, "name": "app", "instances": many}, `manifest.instances has more than 12 entries`)
	vars := M{}
	for i := 0; i < 21; i++ {
		vars["v"+string(rune('a'+i))] = M{"default": "x"}
	}
	parseErr(t, M{"altengine": 1, "name": "app", "vars": vars, "instances": M{"datastore": M{"service": "datastore"}}}, `manifest.vars has more than 20 entries`)
	q := M{}
	for i := 0; i < 21; i++ {
		q["q"+string(rune('a'+i))] = M{}
	}
	parseErr(t, M{"altengine": 1, "name": "app", "instances": M{"tasks": M{"service": "tasks", "queues": q}}}, `queues has more than 20 entries`)
	env, sec := M{}, M{}
	for i := 0; i < 17; i++ {
		env["E"+string(rune('A'+i))] = "x"
		sec["S"+string(rune('A'+i))] = M{}
	}
	parseErr(t, M{"altengine": 1, "name": "app", "instances": M{"functions": M{"service": "functions", "env": env, "secrets": sec}}}, `at most 32 secrets and env values together`)
}

func TestMergeConfig(t *testing.T) {
	current := map[string]any{
		"settings": map[string]any{"allowSignup": false, "allowedOrigins": []any{"https://mine.example"}, "accessTokenTTL": 900.0},
		"access":   map[string]any{"datastore:a": map[string]any{"level": "read"}, "datastore:b": map[string]any{"level": "write"}},
	}
	out := MergeConfig("auth", current, map[string]any{
		"settings": map[string]any{"allowedOrigins": []any{"https://app-web.altengine.app"}},
		"access":   map[string]any{"datastore:a": map[string]any{"level": "write"}},
	})
	wantSettings := map[string]any{"allowSignup": false, "accessTokenTTL": 900.0, "allowedOrigins": []any{"https://mine.example", "https://app-web.altengine.app"}}
	if !reflect.DeepEqual(out["settings"], wantSettings) {
		t.Fatalf("settings = %v", out["settings"])
	}
	if !reflect.DeepEqual(out["access"], map[string]any{"datastore:a": map[string]any{"level": "write"}, "datastore:b": map[string]any{"level": "write"}}) {
		t.Fatalf("access = %v", out["access"])
	}
	changes := map[string]any{"corsOrigins": []any{"https://x.example"}}
	got := MergeConfig("functions", map[string]any{"corsOrigins": []any{"https://y.example"}}, changes)
	if !reflect.DeepEqual(got["corsOrigins"], []any{"https://y.example", "https://x.example"}) {
		t.Fatalf("corsOrigins = %v", got["corsOrigins"])
	}
	if !reflect.DeepEqual(changes["corsOrigins"], []any{"https://x.example"}) {
		t.Fatal("the manifest's own object was changed by the merge")
	}
}

func TestExpandOriginLists(t *testing.T) {
	cfg := map[string]any{"settings": map[string]any{"allowedOrigins": []any{"${console:origin}", "http://localhost:5173", "${console:origin}"}}}
	out := ExpandOriginLists("auth", cfg, map[string][]string{"console": {"http://127.0.0.1:9192"}})
	if got := out["settings"].(map[string]any)["allowedOrigins"]; !reflect.DeepEqual(got, []any{"http://127.0.0.1:9192", "http://localhost:5173"}) {
		t.Fatalf("allowedOrigins = %v", got)
	}
}
