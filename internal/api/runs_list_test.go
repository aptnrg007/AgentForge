package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secondAgentYAML = `
name: second
model:
  provider: ollama
  name: test-model
instructions: you are a second test assistant
limits:
  max_turns: 10
`

func postRun(t *testing.T, ts *httptest.Server, agentName string) runResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/agents/"+agentName+"/run", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST run: status %d: %s", resp.StatusCode, body)
	}
	var out runResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode run response: %v (body=%s)", err, body)
	}
	return out
}

func TestListRunsEndpoint(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory(textResponse("r1"), textResponse("r2"), textResponse("r3")))

	postAgent(t, ts, minimalYAML)
	postAgent(t, ts, secondAgentYAML)

	postRun(t, ts, "minimal")
	postRun(t, ts, "minimal")
	postRun(t, ts, "second")

	var all []runSummary
	if err := getJSON(t, ts.URL+"/v1/runs", &all); err != nil {
		t.Fatalf("GET /v1/runs: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 runs total, got %d: %+v", len(all), all)
	}

	var filtered []runSummary
	if err := getJSON(t, ts.URL+"/v1/runs?agent=minimal", &filtered); err != nil {
		t.Fatalf("GET /v1/runs?agent=minimal: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("expected 2 runs for minimal, got %d: %+v", len(filtered), filtered)
	}
	for _, r := range filtered {
		if r.AgentName != "minimal" {
			t.Fatalf("expected only minimal's runs, got %+v", r)
		}
	}

	var limited []runSummary
	if err := getJSON(t, ts.URL+"/v1/runs?limit=1", &limited); err != nil {
		t.Fatalf("GET /v1/runs?limit=1: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected limit=1 to cap at 1 run, got %d", len(limited))
	}
}

func getJSON(t *testing.T, url string, out any) error {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}
