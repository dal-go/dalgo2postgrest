package dalgo2postgrest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Supabase-style fixture is checked in as data: Compose services, a Kong
// declarative configuration, a seed script and two harnesses. These tests are
// hermetic. They pin the properties that make the stage-2 acceptance honest:
// immutable images, no internal host port, a real Kong ACL refusal, real RLS
// and no write grant, exact-owned teardown, and a consumer pinned to the
// published module rather than this working tree.

func readFixtureFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", "supabase", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(body) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return string(body)
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(body) == 0 {
		t.Fatalf("%s is empty", name)
	}
	return string(body)
}

func requireContains(t *testing.T, name, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s does not contain %q", name, needle)
	}
}

func requireNotContains(t *testing.T, name, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s must not contain %q", name, needle)
	}
}

func TestSupabaseFixtureImageDigestsAreImmutable(t *testing.T) {
	want := map[string]string{
		"postgres:18":               "sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4",
		"postgrest/postgrest:v16.5": "sha256:1347d7552b88f17832912c06afbf7c43209af6eeeb60834b04aae12c9bd9d5cc",
		"kong/kong:3.9.3":           "sha256:9a2ae6699a2ce0d60592eb176555d3594a22782c20cc6557a61ff3a7e8b559a3",
	}
	compose := readFixtureFile(t, "compose.yml")
	images := make(map[string]bool)
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		images[value] = true
	}
	if len(images) != len(want) {
		t.Fatalf("compose.yml declares %d images, want %d: %v", len(images), len(want), images)
	}
	digestPattern := regexp.MustCompile(`^[^@]+@sha256:[0-9a-f]{64}$`)
	for reference, digest := range want {
		value := reference + "@" + digest
		if !images[value] {
			t.Fatalf("compose.yml does not pin %s by its exact digest", value)
		}
		for candidate := range images {
			if !strings.Contains(candidate, "@"+digest) {
				continue
			}
			if !digestPattern.MatchString(candidate) {
				t.Fatalf("image reference is not tag@sha256:...: %q", candidate)
			}
		}
	}
	requireNotContains(t, "compose.yml", compose, ":latest")
}

func TestSupabaseFixturePublishesNoInternalHostPort(t *testing.T) {
	compose := readFixtureFile(t, "compose.yml")
	services := composeServiceBlocks(t, compose)
	for _, name := range []string{"postgres", "postgrest"} {
		block, ok := services[name]
		if !ok {
			t.Fatalf("compose.yml has no %s service", name)
		}
		requireNotContains(t, name+" service", block, "\n    ports:")
		requireNotContains(t, name+" service", block, "\n      - \""+name)
	}
	kong, ok := services["kong"]
	if !ok {
		t.Fatal("compose.yml has no kong service")
	}
	requireContains(t, "kong service", kong, `127.0.0.1:${SUPABASE_KONG_PORT:?SUPABASE_KONG_PORT is required}:8000`)
	requireContains(t, "kong service", kong, `KONG_ADMIN_LISTEN: "off"`)
	requireNotContains(t, "compose.yml", compose, "8001")
	requireNotContains(t, "compose.yml", compose, "127.0.0.1:${SUPABASE_POSTGRES_PORT")
	requireContains(t, "compose.yml", compose, `PGRST_JWT_SECRET: "${SUPABASE_FIXTURE_JWT_SECRET:?SUPABASE_FIXTURE_JWT_SECRET is required}"`)
	requireContains(t, "compose.yml", compose, `PGRST_DB_MAX_ROWS: "37"`)
}

// composeServiceBlocks returns the text of each service under the top-level
// `services:` key. The fixture uses only two-space service names, so a small
// scanner is enough and keeps this test dependency-free.
func composeServiceBlocks(t *testing.T, compose string) map[string]string {
	t.Helper()
	blocks := make(map[string]string)
	lines := strings.Split(compose, "\n")
	inServices := false
	current := ""
	var body []string
	flush := func() {
		if current != "" {
			blocks[current] = strings.Join(body, "\n")
		}
		body = nil
		current = ""
	}
	for _, line := range lines {
		if line == "services:" {
			inServices = true
			continue
		}
		if !inServices {
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			flush()
			inServices = false
			continue
		}
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(line, ":") {
			flush()
			current = strings.TrimSuffix(strings.TrimPrefix(line, "  "), ":")
			continue
		}
		body = append(body, line)
	}
	flush()
	return blocks
}

func TestSupabaseKongRefusesOpenAPIRootToPublicKeyOnly(t *testing.T) {
	kong := readFixtureFile(t, "kong.yml")
	requireContains(t, "kong.yml", kong, `expression: 'http.path == "/rest/v1/"'`)
	requireContains(t, "kong.yml", kong, "allow: [admin]")
	requireContains(t, "kong.yml", kong, "allow: [anon]")
	requireContains(t, "kong.yml", kong, "key: sb_publishable_datatug_dalgo2postgrest_fixture")
	if got := strings.Count(kong, "hide_credentials: false"); got != 2 {
		t.Fatalf("kong.yml has %d key-auth plugins with hide_credentials: false, want 2", got)
	}
	if got := strings.Count(kong, "key:"); got != 1 {
		t.Fatalf("kong.yml declares %d keyauth key lines, want exactly the one synthetic public key", got)
	}
	// The refusal must come from native plugins, not a termination plugin or a
	// transformed request, and no admin/service credential may appear here.
	for _, forbidden := range []string{
		"- name: request-termination",
		"- name: request-transformer",
		"- name: response-transformer",
		"- name: admin-gui",
		"admin_gui",
		"service_role",
		"SUPABASE_SERVICE",
		"eyJhbGciOi",
		"jwt_secret",
	} {
		requireNotContains(t, "kong.yml", kong, forbidden)
	}
}

func TestSupabaseSeedProvesRLSWithoutWriteGrants(t *testing.T) {
	seed := readFixtureFile(t, "seed.sql")
	lower := strings.ToLower(seed)
	requireContains(t, "seed.sql", lower, "alter table app.records enable row level security;")
	requireContains(t, "seed.sql", lower, "current_setting('request.jwt.claims', true)::jsonb ->> 'sub'")
	requireContains(t, "seed.sql", lower, "grant select on app.records to authenticated;")
	requireContains(t, "seed.sql", lower, "from generate_series(1, 250) as g;")
	requireContains(t, "seed.sql", lower, "'other-user'")
	requireContains(t, "seed.sql", lower, "9000 + g")
	requireContains(t, "seed.sql", lower, "9007199254740993")
	for _, forbidden := range []string{
		"grant insert",
		"grant update",
		"grant delete",
		"grant all",
		"grant truncate",
		"service_role",
		"bypassrls",
		"superuser",
	} {
		requireNotContains(t, "seed.sql", lower, forbidden)
	}
	// The restricted relation exists but is never granted to the reading role.
	requireNotContains(t, "seed.sql", lower, "grant select on app.restricted_notes")
	// Only the disposable authenticator role may carry the local password.
	if got := strings.Count(lower, "password 'authenticator-pass'"); got != 1 {
		t.Fatalf("seed.sql declares %d local passwords, want exactly the authenticator's", got)
	}
}

func TestSupabaseLifecycleOwnsExactProject(t *testing.T) {
	lib := readRepoFile(t, filepath.Join("scripts", "lib", "supabase-lifecycle.sh"))
	script := readRepoFile(t, filepath.Join("scripts", "check-supabase-rest.sh"))
	requireContains(t, "lifecycle", lib, "dalgo2postgrest-supabase-")
	requireContains(t, "lifecycle", lib, "down -v --remove-orphans")
	requireContains(t, "lifecycle", lib, "docker ps -aq --filter")
	requireContains(t, "lifecycle", lib, "docker volume ls -q --filter")
	requireContains(t, "lifecycle", lib, "docker network ls -q --filter")
	requireContains(t, "lifecycle", lib, "outside this run's owned prefix")
	requireContains(t, "acceptance script", script, "supabase_resources_absent")
	requireContains(t, "acceptance script", script, "supabase_lifecycle_install")
	requireContains(t, "acceptance script", script, "started=1")
	requireContains(t, "acceptance script", script, "trap - EXIT INT TERM")
	for _, forbidden := range []string{
		"docker system prune",
		"docker volume prune",
		"docker network prune",
		"docker container prune",
		"docker kill",
		"docker ps -q | xargs",
		"docker rm -f $(docker ps",
		"--remove-orphans && ",
	} {
		requireNotContains(t, "acceptance scripts", lib+script, forbidden)
	}
	// The CLI proof is separated from the source proof and names the exact
	// installed artifact it is willing to treat as the installed release.
	requireContains(t, "acceptance script", script, "0.127.0")
	requireContains(t, "acceptance script", script, "4df7e9ce1a2319c01badeabd450703ea72f03993")
	requireContains(t, "acceptance script", script, "4aa2bb538ba890e4d0e4ac069f1e4323a9e56f88824366b26b80293662f3e603")
	requireContains(t, "acceptance script", script, "SUPABASE_SKIP_INSTALLED_CLI")
	requireContains(t, "acceptance script", script, "does NOT claim installed-release acceptance")
	requireContains(t, "acceptance script", script, "DATATUG_POSTGREST_API_KEY")
	requireContains(t, "acceptance script", script, "DATATUG_POSTGREST_BEARER")
	// Verbose output is required by the shipped PASS/SKIP guards below.
	requireContains(t, "acceptance script", script, "go test -race -v -count=1 -run '^TestSupabaseIntegration'")
	requireContains(t, "acceptance script", script, "GOTOOLCHAIN=local")
}

func TestSupabaseLifecycleScriptsAreRunnable(t *testing.T) {
	for _, name := range []string{
		filepath.Join("scripts", "check-supabase-rest.sh"),
		filepath.Join("scripts", "test-supabase-lifecycle.sh"),
	} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("%s is not executable", name)
		}
	}
}

func TestSupabaseConsumerPinsPublishedModule(t *testing.T) {
	goMod := readRepoFile(t, filepath.Join("testdata", "consumer", "go.mod"))
	requireContains(t, "consumer go.mod", goMod, "github.com/dal-go/dalgo2postgrest v0.0.0-20261027200821-d952c8fc3279")
	requireNotContains(t, "consumer go.mod", goMod, "replace ")
	requireNotContains(t, "consumer go.mod", goMod, "=> ../")
	requireNotContains(t, "consumer go.mod", goMod, "=> /")
	if _, err := os.Stat(filepath.Join("testdata", "consumer", "go.sum")); err != nil {
		t.Fatalf("the consumer must commit a go.sum: %v", err)
	}
	testFile := readRepoFile(t, filepath.Join("testdata", "consumer", "supabase_acceptance_test.go"))
	requireContains(t, "consumer test", testFile, `"github.com/dal-go/dalgo2postgrest"`)
	requireContains(t, "consumer test", testFile, "SUPABASE_TEST_KONG_URL")
	requireNotContains(t, "consumer test", testFile, "../../")
}

func TestSupabaseIntegrationTestsAreOptIn(t *testing.T) {
	source := readRepoFile(t, "supabase_integration_test.go")
	requireContains(t, "source integration", source, "SUPABASE_TEST_KONG_URL")
	requireContains(t, "source integration", source, "t.Skip(")
	if strings.Contains(source, "func TestSupabaseIntegration") == false {
		t.Fatal("no TestSupabaseIntegration function is defined")
	}
	// A skipped opt-in test must still fail the acceptance script if the
	// fixture never reaches it; the script checks for PASS lines, not for exit
	// status alone.
	script := readRepoFile(t, filepath.Join("scripts", "check-supabase-rest.sh"))
	requireContains(t, "acceptance script", script, "--- SKIP: TestSupabaseIntegration_")
	requireContains(t, "acceptance script", script, "--- PASS: TestSupabaseIntegration_")
}

func TestExistingPostgRESTFixtureIsUnchanged(t *testing.T) {
	compose := readRepoFile(t, filepath.Join("testdata", "docker-compose.yml"))
	requireContains(t, "testdata/docker-compose.yml", compose, "postgres:18")
	requireContains(t, "testdata/docker-compose.yml", compose, "postgrest/postgrest:v16.5")
	requireContains(t, "testdata/docker-compose.yml", compose, `PGRST_DB_MAX_ROWS: "3"`)
	seed := readRepoFile(t, filepath.Join("testdata", "seed.sql"))
	requireContains(t, "testdata/seed.sql", seed, "create table app.items")
	requireContains(t, "testdata/seed.sql", seed, "9007199254740993")
}
