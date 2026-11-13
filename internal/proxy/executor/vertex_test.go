package executor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testSAJSON(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	sa := map[string]any{
		"type":         "service_account",
		"project_id":   "proj-abc",
		"client_email": "svc@proj-abc.iam.gserviceaccount.com",
		"private_key":  pemText,
	}
	raw, _ := json.Marshal(sa)
	return string(raw)
}

// A service-account key arrives as JSON in the apiKey field, and the lane has to
// recognise it: a raw key goes in the query string while an SA key mints a
// Bearer token, and getting that wrong sends a signed JWT as an API key.
func TestParseVertexServiceAccount(t *testing.T) {
	sa := parseVertexServiceAccount(testSAJSON(t))
	if sa == nil {
		t.Fatal("a valid service-account key was not recognised")
	}
	if sa.ProjectID != "proj-abc" {
		t.Errorf("project = %q, want proj-abc", sa.ProjectID)
	}

	// A plain API key is not JSON at all.
	if got := parseVertexServiceAccount("AIzaSyPlainKey"); got != nil {
		t.Error("a raw API key was taken for a service account")
	}
	// JSON that is not a service account.
	if got := parseVertexServiceAccount(`{"type":"authorized_user"}`); got != nil {
		t.Error("an ADC credential was taken for a service account")
	}
	// An SA key missing a required field is not usable.
	if got := parseVertexServiceAccount(`{"type":"service_account","client_email":"x@y"}`); got != nil {
		t.Error("an incomplete service account was accepted")
	}
}

func TestParseVertexADC(t *testing.T) {
	adc := parseVertexADC(`{"type":"authorized_user","client_id":"cid","client_secret":"cs","refresh_token":"rt","quota_project_id":"p1"}`)
	if adc == nil {
		t.Fatal("an ADC credential was not recognised")
	}
	if adc.QuotaProject != "p1" {
		t.Errorf("quota project = %q, want p1", adc.QuotaProject)
	}
	if got := parseVertexADC(`{"type":"service_account"}`); got != nil {
		t.Error("a service account was taken for an ADC credential")
	}
	if got := parseVertexADC(`{"type":"authorized_user","refresh_token":"rt"}`); got != nil {
		t.Error("an ADC credential without a client_id was accepted")
	}
}

// The assertion is a real RS256 JWT: three dot-separated segments, the claims
// Google requires, and a header naming RS256.
func TestVertexJWTAssertionShape(t *testing.T) {
	sa := parseVertexServiceAccount(testSAJSON(t))
	assertion, err := vertexJWTAssertion(sa)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3: %s", len(parts), assertion)
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if !strings.Contains(string(header), `"RS256"`) {
		t.Errorf("header does not name RS256: %s", header)
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(claims, &parsed); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	if parsed["iss"] != sa.ClientEmail {
		t.Errorf("iss = %v, want %q", parsed["iss"], sa.ClientEmail)
	}
	if parsed["aud"] != vertexGoogleTokenURL {
		t.Errorf("aud = %v, want %q", parsed["aud"], vertexGoogleTokenURL)
	}
	if parsed["scope"] != vertexCloudScope {
		t.Errorf("scope = %v, want %q", parsed["scope"], vertexCloudScope)
	}
	if _, ok := parsed["exp"]; !ok {
		t.Error("claims carry no exp")
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		t.Errorf("signature segment is not valid base64url: %v", err)
	}
}

// A key that arrives with literal backslash-n (a JSON-in-JSON paste) still
// parses: refusing it would fail the one shape most operators paste.
func TestVertexJWTAssertionAcceptsEscapedNewlines(t *testing.T) {
	sa := parseVertexServiceAccount(testSAJSON(t))
	sa.PrivateKey = strings.ReplaceAll(sa.PrivateKey, "\n", `\n`)
	if _, err := vertexJWTAssertion(sa); err != nil {
		t.Errorf("escaped newlines were rejected: %v", err)
	}
}

func TestVertexJWTAssertionRejectsGarbage(t *testing.T) {
	if _, err := vertexJWTAssertion(&vertexServiceAccount{ClientEmail: "x", PrivateKey: "not pem"}); err == nil {
		t.Error("expected an error for a non-PEM key")
	}
}

func TestVertexModelStripsPrefixAndSuffix(t *testing.T) {
	cases := map[string]string{
		"vertex/gemini-4-argon":     "gemini-4-argon",
		"vx/gemini-3.1-pro-preview": "gemini-3.1-pro-preview",
		"gemini-4-argon-thinking":   "gemini-4-argon",
		"gemini-4-argon(high)":      "gemini-4-argon",
	}
	for in, want := range cases {
		if got := vertexModel(in); got != want {
			t.Errorf("vertexModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// vertexCaptureTransport answers the Google token endpoint and records the
// aiplatform URL the executor dials, so URL routing is pinned without network.
type vertexCaptureTransport struct {
	lastURL  string
	lastAuth string
}

func (t *vertexCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "oauth2.googleapis.com" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"tok-test","expires_in":3600}`)),
			Header:     make(http.Header),
		}, nil
	}
	t.lastURL = req.URL.String()
	t.lastAuth = req.Header.Get("Authorization")
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"candidates":[]}`)),
		Header:     make(http.Header),
	}, nil
}

func testForwardVertex(t *testing.T, req *Request) *vertexCaptureTransport {
	t.Helper()
	tr := &vertexCaptureTransport{}
	req.Client = &http.Client{Transport: tr}
	if req.Ctx == nil {
		req.Ctx = context.Background()
	}
	rec := httptest.NewRecorder()
	if err := ForwardVertex(rec, req); err != nil {
		t.Fatalf("ForwardVertex: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return tr
}

func vertexOpenAIBody(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
}

// Argon (Gemini 4) serves only from the global multi-region: an OAuth turn
// with no configured location must dial global, not us-central1 (fork rule,
// ported from the old BuildVertexURL tests).
func TestForwardVertexArgonOAuthDefaultsToGlobal(t *testing.T) {
	tr := testForwardVertex(t, &Request{
		Provider:  "vertex",
		APIKey:    testSAJSON(t),
		ModelName: "gemini-4-argon",
		Body:      vertexOpenAIBody("gemini-4-argon"),
	})
	if !strings.Contains(tr.lastURL, "/locations/global/") {
		t.Errorf("want global location, got %s", tr.lastURL)
	}
	if tr.lastAuth != "Bearer tok-test" {
		t.Errorf("auth = %q, want Bearer tok-test", tr.lastAuth)
	}
	if strings.Contains(tr.lastURL, "key=") {
		t.Errorf("oauth URL must not carry ?key=: %s", tr.lastURL)
	}
}

// Raw API-key routing: an explicit project honors its location (default
// global), and without a project the projectless endpoint is kept.
func TestForwardVertexRawKeyLocations(t *testing.T) {
	tr := testForwardVertex(t, &Request{
		Provider:  "vertex",
		APIKey:    "test-key",
		ModelName: "gemini-2.5-flash",
		Body:      vertexOpenAIBody("gemini-2.5-flash"),
		ConnData:  map[string]any{"projectId": "test-project", "location": "eu"},
	})
	if !strings.Contains(tr.lastURL, "/locations/eu/") {
		t.Errorf("want eu location, got %s", tr.lastURL)
	}

	tr = testForwardVertex(t, &Request{
		Provider:  "vertex",
		APIKey:    "test-key",
		ModelName: "gemini-2.5-flash",
		Body:      vertexOpenAIBody("gemini-2.5-flash"),
		ConnData:  map[string]any{"projectId": "test-project"},
	})
	if !strings.Contains(tr.lastURL, "/locations/global/") || !strings.Contains(tr.lastURL, "key=test-key") {
		t.Errorf("want global + ?key=, got %s", tr.lastURL)
	}

	tr = testForwardVertex(t, &Request{
		Provider:  "vertex",
		APIKey:    "test-key",
		ModelName: "gemini-2.5-flash",
		Body:      vertexOpenAIBody("gemini-2.5-flash"),
	})
	want := "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-2.5-flash:generateContent?key=test-key"
	if tr.lastURL != want {
		t.Errorf("got %s, want %s", tr.lastURL, want)
	}
}

// The partner lane is the global OpenAI-compatible endpoint and requires a project.
func TestForwardVertexPartnerEndpoint(t *testing.T) {
	tr := testForwardVertex(t, &Request{
		Provider:  "vertex-partner",
		APIKey:    "test-key",
		ModelName: "partner-model",
		Body:      vertexOpenAIBody("partner-model"),
		ConnData:  map[string]any{"projectId": "test-project"},
	})
	want := "https://aiplatform.googleapis.com/v1/projects/test-project/locations/global/endpoints/openapi/chat/completions?key=test-key"
	if tr.lastURL != want {
		t.Errorf("got %s, want %s", tr.lastURL, want)
	}

	rec := httptest.NewRecorder()
	err := ForwardVertex(rec, &Request{
		Provider:  "vertex-partner",
		APIKey:    "test-key",
		ModelName: "partner-model",
		Body:      vertexOpenAIBody("partner-model"),
		Ctx:       context.Background(),
		Client:    &http.Client{Transport: &vertexCaptureTransport{}},
	})
	if err == nil {
		t.Error("partner without project must fail")
	}
}

// Both entries resolve to an executor.
func TestVertexExecutorRegistered(t *testing.T) {
	RegisterAll()
	if Get("vertex") == nil {
		t.Error("vertex executor not registered")
	}
	if Get("vertex-partner") == nil {
		t.Error("vertex-partner executor not registered")
	}
}
