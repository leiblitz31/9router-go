package executor

import (
	"net/url"
	"strings"
	"testing"
)

// TestBuildVertexURL_APIKeyLocations is the Go port of upstream's
// tests/unit/vertex-location.test.js (#4821): raw API-key connections with an
// explicit project honor location (default global); without a project the
// projectless endpoint is kept.
func TestBuildVertexURL_APIKeyLocations(t *testing.T) {
	for _, stream := range []bool{false, true} {
		creds := VertexCredentials{
			APIKey: "test-key", ProjectID: "test-project", Location: "global",
		}
		got, err := BuildVertexURL("gemini-4-argon", stream, creds)
		if err != nil {
			t.Fatalf("stream=%v: %v", stream, err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse %q: %v", got, err)
		}
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		want := "/v1/projects/test-project/locations/global/publishers/google/models/gemini-4-argon:" + action
		if u.Hostname() != "aiplatform.googleapis.com" || u.Path != want {
			t.Errorf("stream=%v: path = %s, want %s", stream, u.Path, want)
		}
		if u.Query().Get("key") != "test-key" {
			t.Errorf("stream=%v: missing ?key=: %s", stream, got)
		}
		if stream && u.Query().Get("alt") != "sse" {
			t.Errorf("stream: missing ?alt=sse: %s", got)
		}
		if !stream && u.Query().Get("alt") != "" {
			t.Errorf("non-stream must not carry alt: %s", got)
		}
	}

	t.Run("explicit project without location defaults to global", func(t *testing.T) {
		got, err := BuildVertexURL("gemini-4-argon", false, VertexCredentials{APIKey: "test-key", ProjectID: "test-project"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "/projects/test-project/locations/global/") {
			t.Errorf("want global default: %s", got)
		}
	})

	t.Run("honors explicitly configured locations", func(t *testing.T) {
		got, err := BuildVertexURL("gemini-2.5-flash", false, VertexCredentials{APIKey: "test-key", ProjectID: "test-project", Location: "eu"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "/locations/eu/") {
			t.Errorf("want eu: %s", got)
		}
	})

	t.Run("projectless endpoint preserved for existing connections", func(t *testing.T) {
		got, err := BuildVertexURL("gemini-2.5-flash", false, VertexCredentials{APIKey: "test-key"})
		if err != nil {
			t.Fatal(err)
		}
		want := "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-2.5-flash:generateContent?key=test-key"
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}

// TestBuildVertexURL_OAuthRouting ports the OAuth cases: regional routing and
// bearer auth are preserved, and global-only models (Argon) default to global
// instead of us-central1 (29fc880e).
func TestBuildVertexURL_OAuthRouting(t *testing.T) {
	t.Run("regional routing and no key param", func(t *testing.T) {
		creds := VertexCredentials{AccessToken: "test-token", ProjectID: "test-project", Location: "us-central1"}
		got, err := BuildVertexURL("gemini-2.5-flash", false, creds)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(got)
		if !strings.Contains(u.Path, "/locations/us-central1/") {
			t.Errorf("want us-central1: %s", got)
		}
		if u.Query().Has("key") {
			t.Errorf("oauth URL must not carry ?key=: %s", got)
		}
		if h := BuildVertexHeaders(creds, false); h["Authorization"] != "Bearer test-token" {
			t.Errorf("headers = %v", h)
		}
	})

	t.Run("argon defaults OAuth to global", func(t *testing.T) {
		creds := VertexCredentials{AccessToken: "test-token", ProjectID: "test-project"}
		got, err := BuildVertexURL("gemini-4-argon", false, creds)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "/locations/global/") {
			t.Errorf("want global: %s", got)
		}
	})

	t.Run("oauth without project fails", func(t *testing.T) {
		if _, err := BuildVertexURL("gemini-2.5-flash", false, VertexCredentials{AccessToken: "t"}); err == nil {
			t.Error("expected error")
		}
	})
}

// TestBuildVertexURL_PartnerEndpoint ports the partner case: project required,
// global OpenAI-compatible endpoint, key preserved.
func TestBuildVertexURL_PartnerEndpoint(t *testing.T) {
	creds := VertexCredentials{Provider: "vertex-partner", APIKey: "test-key", ProjectID: "test-project", Location: "eu"}
	got, err := BuildVertexURL("partner-model", false, creds)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://aiplatform.googleapis.com/v1/projects/test-project/locations/global/endpoints/openapi/chat/completions?key=test-key"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if _, err := BuildVertexURL("partner-model", false, VertexCredentials{Provider: "vertex-partner", APIKey: "k"}); err == nil {
		t.Error("partner without project must fail")
	}
}

// TestVertexExecutorRegistered guards the wiring: both entries resolve.
func TestVertexExecutorRegistered(t *testing.T) {
	RegisterAll()
	if Get("vertex") == nil {
		t.Error("vertex executor not registered")
	}
	if Get("vertex-partner") == nil {
		t.Error("vertex-partner executor not registered")
	}
}
