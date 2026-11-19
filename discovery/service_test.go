package discovery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func fixtureService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "mapping.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(d, nil)
	t.Cleanup(func() { s.Close(); _ = d.Close() })
	return s, d
}
func TestMappingUsesNativeProvidersAndMergesSources(t *testing.T) {
	const key = "mapping-fixture-secret"
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fofa":
			if r.URL.Query().Get("key") != key {
				t.Error("FOFA credential not forwarded")
			}
			query, _ := base64.StdEncoding.DecodeString(r.URL.Query().Get("qbase64"))
			if string(query) != `domain="example.test"` {
				t.Errorf("FOFA query=%q", query)
			}
			_, _ = io.WriteString(w, `{"error":false,"size":1,"results":[["https://a.example.test","203.0.113.11","443","https","example.test","fixture","UK","AS1","Org"]]}`)
		case "/hunter":
			if r.URL.Query().Get("api-key") != key {
				t.Error("Hunter credential not forwarded")
			}
			query, _ := base64.URLEncoding.DecodeString(r.URL.Query().Get("search"))
			if string(query) != `domain="example.test"` {
				t.Errorf("Hunter query=%q", query)
			}
			_, _ = io.WriteString(w, `{"code":200,"data":{"total":1,"arr":[{"url":"https://a.example.test","ip":"203.0.113.11","port":443,"domain":"example.test","web_title":"fixture","protocol":"https"}]}}`)
		case "/quake":
			if r.Header.Get("X-QuakeToken") != key {
				t.Error("Quake credential not forwarded")
			}
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["query"] != `domain:"example.test"` {
				t.Errorf("Quake query=%v", req["query"])
			}
			_, _ = io.WriteString(w, `{"code":0,"data":[{"ip":"203.0.113.11","port":443,"hostname":"a.example.test","domain":"example.test","service":{"name":"https","http":{"title":"fixture","status_code":200}}}],"meta":{"pagination":{"total":1}}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	service, _ := fixtureService(t)
	on := true
	for _, id := range providers {
		endpoint := api.URL + "/" + id
		if _, err := service.Update(id, Patch{Key: ptr(key), Enabled: &on, Endpoint: &endpoint}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := service.Search(t.Context(), Input{Query: "example.test", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(out.Results) != 1 || len(out.Results[0].Sources) != 3 {
		t.Fatalf("output=%+v calls=%d", out, calls.Load())
	}
	if out.Results[0].Host != "a.example.test" || out.Results[0].IP != "203.0.113.11" {
		t.Fatalf("native result=%+v", out.Results[0])
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), key) {
		t.Fatal("credential leaked into tool output")
	}
	if _, err := service.Search(t.Context(), Input{Query: "domain=example.test", Native: true, Provider: "all"}); err == nil {
		t.Fatal("native all-source query was accepted")
	}
}
func ptr[T any](v T) *T { return &v }
func TestMappingSettingsPreserveSecretsAndDoNotPersistEnvironmentKeys(t *testing.T) {
	const envKey = "env-only-fixture"
	const storedKey = "stored-key-fixture"
	t.Setenv("ARTEX_FOFA_KEY", envKey)
	service, d := fixtureService(t)
	views, err := service.Sources()
	if err != nil {
		t.Fatal(err)
	}
	if !views[0].Enabled || !views[0].KeySet || !views[0].KeyFromEnv {
		t.Fatalf("environment view=%+v", views[0])
	}
	if _, err := service.Update("fofa", Patch{TimeoutSeconds: ptr(45)}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := d.GetSetting("mapping.source.fofa")
	if err != nil || strings.Contains(raw, envKey) {
		t.Fatalf("environment secret persisted: %v", err)
	}
	if _, err := service.Update("fofa", Patch{Key: ptr(storedKey), Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update("fofa", Patch{Email: ptr("operator@example.test")}); err != nil {
		t.Fatal(err)
	}
	config, err := service.load("fofa")
	if err != nil || config.Key != storedKey || !config.Enabled {
		t.Fatalf("partial save lost credential: %+v %v", config, err)
	}
	views, err = service.Sources()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(views)
	if strings.Contains(string(encoded), envKey) || strings.Contains(string(encoded), storedKey) {
		t.Fatal("source API returned secret")
	}
	if views[0].KeyFromEnv {
		t.Fatal("stored key did not override environment fallback")
	}
	if _, err := service.Update("fofa", Patch{Key: ptr(""), Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(t.Context(), Input{Provider: "fofa", Query: "example.test"}); err == nil {
		t.Fatal("disabled source accepted")
	}
}
func TestMappingCloseCancelsActiveSDKQueryAndRedactsProviderErrors(t *testing.T) {
	service, _ := fixtureService(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer api.Close()
	if _, err := service.Update("fofa", Patch{Key: ptr("secret"), Enabled: ptr(true), Endpoint: ptr(api.URL)}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Search(context.Background(), Input{Provider: "fofa", Query: "example.test"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("SDK query did not reach fixture")
	}
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("mapping shutdown blocked")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("query outlived shutdown")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("query did not exit")
	}
}
