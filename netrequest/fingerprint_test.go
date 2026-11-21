package netrequest

import (
	"net/http"
	"testing"

	"github.com/emhha/netloom/httpdata"
	"github.com/emhha/netloom/passive"
)

func TestEnabledFingerprintSourcesWithoutRuleDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	service, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	response := &httpdata.Response{
		Header: http.Header{"Server": {"nginx/1.24.0"}},
		Body:   []byte("/seeyon/USER-DATA/IMAGES/LOGIN/login.gif"),
	}
	results, err := service.passive.Analyze(t.Context(), passive.Input{
		Request:  &httpdata.Request{Method: "GET", URL: "http://fixture.invalid/"},
		Response: response,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, result := range results {
		if result.PluginID != "fingerprint" {
			continue
		}
		if result.Status != "completed" {
			t.Fatalf("fingerprint analysis failed: %+v", result)
		}
		for _, artifact := range result.Artifacts {
			found[artifact.Rule+":"+artifact.Value] = true
			if artifact.Rule != "ehole" && artifact.Rule != "wappalyzer" {
				t.Fatalf("unselected source: %+v", artifact)
			}
		}
	}
	for _, key := range []string{"ehole:致远OA", "wappalyzer:Nginx"} {
		if !found[key] {
			t.Fatalf("missing %s: %v", key, found)
		}
	}
}
