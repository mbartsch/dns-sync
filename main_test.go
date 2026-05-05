package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTransport rewrites every request to point at the given test server.
type testTransport struct{ host string }

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Host = t.host
	req2.URL.Scheme = "http"
	return http.DefaultTransport.RoundTrip(req2)
}

func newTestDockerClient(srv *httptest.Server, cfg Config) *dockerClient {
	return &dockerClient{
		hc:  &http.Client{Transport: &testTransport{host: strings.TrimPrefix(srv.URL, "http://")}},
		cfg: cfg,
	}
}

// ---------------------------------------------------------------------------
// versionString
// ---------------------------------------------------------------------------

func TestVersionString_NoCommit(t *testing.T) {
	origV, origC := version, commit
	version, commit = "1.2.3", ""
	defer func() { version, commit = origV, origC }()
	if got := versionString(); got != "1.2.3" {
		t.Errorf("got %q, want %q", got, "1.2.3")
	}
}

func TestVersionString_WithCommit(t *testing.T) {
	origV, origC := version, commit
	version, commit = "1.2.3", "abc1234"
	defer func() { version, commit = origV, origC }()
	if got := versionString(); got != "1.2.3-abc1234" {
		t.Errorf("got %q, want %q", got, "1.2.3-abc1234")
	}
}

// ---------------------------------------------------------------------------
// stripQuotes
// ---------------------------------------------------------------------------

func TestStripQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"hello"`, "hello"},
		{`'world'`, "world"},
		{`"no end`, `"no end`},
		{`plain`, `plain`},
		{`""`, ""},
		{`''`, ""},
		{``, ``},
		{`"mixed'`, `"mixed'`},
	}
	for _, c := range cases {
		if got := stripQuotes(c.in); got != c.want {
			t.Errorf("stripQuotes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// envOr
// ---------------------------------------------------------------------------

func TestEnvOr_Present(t *testing.T) {
	t.Setenv("DNS_TEST_KEY", "myval")
	if got := envOr("DNS_TEST_KEY", "fallback"); got != "myval" {
		t.Errorf("got %q, want %q", got, "myval")
	}
}

func TestEnvOr_Missing(t *testing.T) {
	t.Setenv("DNS_TEST_KEY", "")
	if got := envOr("DNS_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}

func TestEnvOr_Quoted(t *testing.T) {
	t.Setenv("DNS_TEST_KEY", `"quoted"`)
	if got := envOr("DNS_TEST_KEY", "fallback"); got != "quoted" {
		t.Errorf("got %q, want %q", got, "quoted")
	}
}

// ---------------------------------------------------------------------------
// envInt
// ---------------------------------------------------------------------------

func TestEnvInt_Present(t *testing.T) {
	t.Setenv("DNS_TEST_INT", "42")
	if got := envInt("DNS_TEST_INT", 0); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestEnvInt_Missing(t *testing.T) {
	t.Setenv("DNS_TEST_INT", "")
	if got := envInt("DNS_TEST_INT", 99); got != 99 {
		t.Errorf("got %d, want 99", got)
	}
}

func TestEnvInt_Invalid(t *testing.T) {
	t.Setenv("DNS_TEST_INT", "notanumber")
	if got := envInt("DNS_TEST_INT", 5); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}

func TestEnvInt_Quoted(t *testing.T) {
	t.Setenv("DNS_TEST_INT", `"30"`)
	if got := envInt("DNS_TEST_INT", 0); got != 30 {
		t.Errorf("got %d, want 30", got)
	}
}

// ---------------------------------------------------------------------------
// dnsRecord.JSON
// ---------------------------------------------------------------------------

func TestDNSRecordJSON(t *testing.T) {
	rec := dnsRecord{A: []aRecord{{TTL: 30, IP: "10.0.0.1"}}}
	want := `{"a":[{"ttl":30,"ip":"10.0.0.1"}]}`
	if got := rec.JSON(); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestDNSRecordJSON_Empty(t *testing.T) {
	if got := (dnsRecord{}).JSON(); got != `{}` {
		t.Errorf("got %s, want {}", got)
	}
}

func TestDNSRecordJSON_MultipleA(t *testing.T) {
	rec := dnsRecord{A: []aRecord{{TTL: 10, IP: "1.1.1.1"}, {TTL: 20, IP: "2.2.2.2"}}}
	got := rec.JSON()
	if !strings.Contains(got, `"1.1.1.1"`) || !strings.Contains(got, `"2.2.2.2"`) {
		t.Errorf("missing IPs in JSON: %s", got)
	}
}

// ---------------------------------------------------------------------------
// secretOrEnv
// ---------------------------------------------------------------------------

func TestSecretOrEnv_Plain(t *testing.T) {
	t.Setenv("DNS_TEST_SECRET", "s3cr3t")
	t.Setenv("DNS_TEST_SECRET_FILE", "")
	if got := secretOrEnv("DNS_TEST_SECRET"); got != "s3cr3t" {
		t.Errorf("got %q, want %q", got, "s3cr3t")
	}
}

func TestSecretOrEnv_File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(p, []byte("filepass\n"), 0600)
	t.Setenv("DNS_TEST_SECRET_FILE", p)
	t.Setenv("DNS_TEST_SECRET", "")
	if got := secretOrEnv("DNS_TEST_SECRET"); got != "filepass" {
		t.Errorf("got %q, want %q", got, "filepass")
	}
}

func TestSecretOrEnv_Empty(t *testing.T) {
	t.Setenv("DNS_TEST_SECRET", "")
	t.Setenv("DNS_TEST_SECRET_FILE", "")
	if got := secretOrEnv("DNS_TEST_SECRET"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// readStaticRecords
// ---------------------------------------------------------------------------

func TestReadStaticRecords_YAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "records.yaml")
	os.WriteFile(p, []byte("ns1:\n  a:\n    - ttl: 30\n      ip: 1.2.3.4\n"), 0644)
	recs, err := readStaticRecords(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := recs["ns1"]; !ok {
		t.Error("expected key 'ns1'")
	}
}

func TestReadStaticRecords_YML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "records.yml")
	os.WriteFile(p, []byte("host1:\n  a:\n    - ttl: 60\n      ip: 10.0.0.1\n"), 0644)
	recs, err := readStaticRecords(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := recs["host1"]; !ok {
		t.Error("expected key 'host1'")
	}
}

func TestReadStaticRecords_JSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "records.json")
	os.WriteFile(p, []byte(`{"@":{"soa":{"ttl":300}}}`), 0644)
	recs, err := readStaticRecords(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := recs["@"]; !ok {
		t.Error("expected key '@'")
	}
}

func TestReadStaticRecords_UnknownExtFallsBackToYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "records.conf")
	os.WriteFile(p, []byte("key1:\n  a:\n    - ttl: 10\n      ip: 5.5.5.5\n"), 0644)
	recs, err := readStaticRecords(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := recs["key1"]; !ok {
		t.Error("expected key 'key1'")
	}
}

func TestReadStaticRecords_NotFound(t *testing.T) {
	_, err := readStaticRecords("/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadStaticRecords_InvalidYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	// A YAML sequence cannot unmarshal into map[string]interface{}.
	os.WriteFile(p, []byte("- item1\n- item2\n"), 0644)
	_, err := readStaticRecords(p)
	if err == nil {
		t.Error("expected error for non-mapping YAML")
	}
}

func TestReadStaticRecords_MultipleKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "multi.yaml")
	os.WriteFile(p, []byte("a:\n  x: 1\nb:\n  y: 2\n"), 0644)
	recs, err := readStaticRecords(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("got %d records, want 2", len(recs))
	}
}

// ---------------------------------------------------------------------------
// Syncer key helpers
// ---------------------------------------------------------------------------

func TestSyncerKeys(t *testing.T) {
	s := &Syncer{cfg: Config{
		RedisPrefix: "_dns:",
		Zone:        "example.com.",
		Hostname:    "myhost",
	}}
	if got := s.zoneKey(); got != "_dns:example.com." {
		t.Errorf("zoneKey = %q", got)
	}
	if got := s.trackingKey(); got != "_dns:tracking:myhost" {
		t.Errorf("trackingKey = %q", got)
	}
}

// ---------------------------------------------------------------------------
// dockerClient.listContainers
// ---------------------------------------------------------------------------

func TestListContainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			fmt.Fprint(w, `[{"Id":"abc123"},{"Id":"def456"}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ids, err := newTestDockerClient(srv, Config{}).listContainers(context.Background())
	if err != nil {
		t.Fatalf("listContainers: %v", err)
	}
	if len(ids) != 2 || ids[0] != "abc123" || ids[1] != "def456" {
		t.Errorf("unexpected ids: %v", ids)
	}
}

func TestListContainers_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	ids, err := newTestDockerClient(srv, Config{}).listContainers(context.Background())
	if err != nil {
		t.Fatalf("listContainers: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %d ids, want 0", len(ids))
	}
}

func TestListContainers_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestDockerClient(srv, Config{}).listContainers(context.Background())
	if err == nil {
		t.Error("expected error for HTTP 500")
	}
}

// ---------------------------------------------------------------------------
// dockerClient.inspectContainer
// ---------------------------------------------------------------------------

func TestInspectContainer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"Name": "/mycontainer",
			"NetworkSettings": {
				"Networks": {"bridge": {"IPAddress": "172.17.0.2"}}
			}
		}`)
	}))
	defer srv.Close()

	name, ip, err := newTestDockerClient(srv, Config{}).inspectContainer(context.Background(), "someid")
	if err != nil {
		t.Fatalf("inspectContainer: %v", err)
	}
	if name != "mycontainer" {
		t.Errorf("name = %q, want mycontainer", name)
	}
	if ip != "172.17.0.2" {
		t.Errorf("ip = %q, want 172.17.0.2", ip)
	}
}

func TestInspectContainer_PreferredNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"Name": "/c",
			"NetworkSettings": {
				"Networks": {
					"bridge": {"IPAddress": "172.17.0.2"},
					"mynet":  {"IPAddress": "10.0.0.5"}
				}
			}
		}`)
	}))
	defer srv.Close()

	_, ip, err := newTestDockerClient(srv, Config{Network: "mynet"}).inspectContainer(context.Background(), "id")
	if err != nil {
		t.Fatalf("inspectContainer: %v", err)
	}
	if ip != "10.0.0.5" {
		t.Errorf("ip = %q, want 10.0.0.5 (preferred network)", ip)
	}
}

func TestInspectContainer_PreferredNetworkMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"Name": "/c",
			"NetworkSettings": {
				"Networks": {"bridge": {"IPAddress": "172.17.0.2"}}
			}
		}`)
	}))
	defer srv.Close()

	// preferred network not present — should fall back to any available IP
	_, ip, err := newTestDockerClient(srv, Config{Network: "missing"}).inspectContainer(context.Background(), "id")
	if err != nil {
		t.Fatalf("inspectContainer: %v", err)
	}
	if ip != "172.17.0.2" {
		t.Errorf("ip = %q, want 172.17.0.2 (fallback)", ip)
	}
}

func TestInspectContainer_NoIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Name": "/noip", "NetworkSettings": {"Networks": {}}}`)
	}))
	defer srv.Close()

	_, _, err := newTestDockerClient(srv, Config{}).inspectContainer(context.Background(), "id")
	if err == nil {
		t.Error("expected error for container with no IP")
	}
}

func TestInspectContainer_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := newTestDockerClient(srv, Config{}).inspectContainer(context.Background(), "id")
	if err == nil {
		t.Error("expected error for HTTP 404")
	}
}
