package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"9router/proxy/internal/constants"
	"9router/proxy/internal/proxy"
	"9router/proxy/internal/translator"
)

// vertexDefaultLocation is the fallback location for OAuth/SA/ADC
// connections when neither providerSpecificData.location nor the model
// registry declares one.
const vertexDefaultLocation = "us-central1"

// vertexGlobalLocation is the default for raw API-key connections with an
// explicit project, and for models the registry marks global-only (Argon).
const vertexGlobalLocation = "global"

// VertexModelDefaultLocation mirrors the registry's per-model defaultLocation
// (open-sse/providers/registry/vertex.js): Argon serves only from the
// global/us/eu multi-regions, never from a regional location.
func VertexModelDefaultLocation(model string) string {
	if translator.IsGemini4Model(strings.ToLower(model)) {
		return vertexGlobalLocation
	}
	return ""
}

// VertexCredentials carries the auth material a Vertex request is built from.
type VertexCredentials struct {
	APIKey               string
	AccessToken          string
	ProjectID            string
	Location             string
	Provider             string // "vertex" or "vertex-partner"
	IsServiceAccountJSON bool   // apiKey holds an SA JSON blob
	IsADCJSON            bool   // apiKey holds an ADC authorized_user blob
}

// VertexCredentialsFromRequest derives credentials from an executor request:
// the OAuth access token wins over the raw key (matching buildHeaders), and
// project/location come from providerSpecificData.
func VertexCredentialsFromRequest(req *Request, provider string) VertexCredentials {
	creds := VertexCredentials{Provider: provider}
	if req == nil {
		return creds
	}
	creds.APIKey = req.APIKey
	if req.ConnData != nil {
		creds.ProjectID = vertexStringField(req.ConnData, "projectId")
		creds.Location = vertexStringField(req.ConnData, "location")
	}
	return creds
}

// VertexCredentialsWithToken returns creds with the access token set, so
// BuildVertexURL routes the OAuth path and BuildVertexHeaders sends Bearer.
func VertexCredentialsWithToken(creds VertexCredentials, token string) VertexCredentials {
	creds.AccessToken = token
	return creds
}

func vertexStringField(connData map[string]any, key string) string {
	if connData == nil {
		return ""
	}
	s, _ := connData[key].(string)
	return strings.TrimSpace(s)
}

// UsesOAuth reports whether the credentials take the OAuth path (SA JSON,
// ADC JSON, or a pre-set access token).
func (c VertexCredentials) UsesOAuth() bool {
	return c.IsServiceAccountJSON || c.IsADCJSON || c.AccessToken != ""
}

// BuildVertexURL constructs the Vertex AI endpoint for a model, mirroring
// VertexExecutor.buildUrl in open-sse/executors/vertex.js (#4821, 29fc880e):
//
//   - vertex-partner always uses the global OpenAI-compatible endpoint and
//     requires a project.
//   - OAuth/SA/ADC requires a project and honors providerSpecificData.location,
//     falling back to the registry defaultLocation (Argon → global) and then
//     us-central1.
//   - Raw API keys with an explicit project honor its location (default
//     global); without a project the projectless publishers endpoint is kept
//     for existing connections.
func BuildVertexURL(model string, stream bool, creds VertexCredentials) (string, error) {
	provider := creds.Provider
	if provider == "" {
		provider = "vertex"
	}
	projectID := creds.ProjectID
	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}

	if provider == "vertex-partner" {
		if projectID == "" {
			return "", fmt.Errorf("vertex partner models require a project_id: set providerSpecificData.projectId")
		}
		url := fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/global/endpoints/openapi/chat/completions", projectID)
		if rawKey := vertexRawKey(creds); rawKey != "" {
			url += "?key=" + rawKey
		}
		return url, nil
	}

	if creds.UsesOAuth() {
		if projectID == "" {
			return "", fmt.Errorf("vertex OAuth/ADC requires a project_id: add quota_project_id to your ADC JSON or set providerSpecificData.projectId")
		}
		location := creds.Location
		if location == "" {
			location = VertexModelDefaultLocation(model)
		}
		if location == "" {
			location = vertexDefaultLocation
		}
		url := fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:%s", projectID, location, model, action)
		if stream {
			url += "?alt=sse"
		}
		return url, nil
	}

	location := creds.Location
	if location == "" {
		location = vertexGlobalLocation
	}
	var url string
	if projectID != "" {
		url = fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:%s", projectID, location, model, action)
	} else {
		url = fmt.Sprintf("https://aiplatform.googleapis.com/v1/publishers/google/models/%s:%s", model, action)
	}
	if stream {
		url += "?alt=sse"
	}
	if rawKey := vertexRawKey(creds); rawKey != "" {
		if stream {
			url += "&key=" + rawKey
		} else {
			url += "?key=" + rawKey
		}
	}
	return url, nil
}

func vertexRawKey(creds VertexCredentials) string {
	if creds.UsesOAuth() {
		return ""
	}
	return creds.APIKey
}

// BuildVertexHeaders builds the request headers: Bearer only when an access
// token is present (a raw key rides in the URL ?key=).
func BuildVertexHeaders(creds VertexCredentials, stream bool) map[string]string {
	headers := map[string]string{"Content-Type": "application/json"}
	if creds.AccessToken != "" {
		headers["Authorization"] = "Bearer " + creds.AccessToken
	}
	if stream {
		headers["Accept"] = "text/event-stream"
	}
	return headers
}

// ForwardVertex forwards an OpenAI-format request to Vertex AI's Gemini
// endpoint. The body is translated OpenAI → Gemini native first; the response
// continues through the existing Gemini handlers so stream and non-stream
// behave like every other Gemini executor.
func ForwardVertex(w http.ResponseWriter, req *Request) error {
	return forwardVertexAs(w, req, "vertex")
}

// ForwardVertexPartner forwards to the Vertex partner endpoint (global
// OpenAI-compatible URL). Registered for "vertex-partner" so the entry's
// provider id — not a Format flag — decides the wire shape, the way one
// Bedrock executor serves both Bedrock entries.
func ForwardVertexPartner(w http.ResponseWriter, req *Request) error {
	return forwardVertexAs(w, req, "vertex-partner")
}

func forwardVertexAs(w http.ResponseWriter, req *Request, provider string) error {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	creds := VertexCredentialsFromRequest(req, provider)

	model := req.ModelName
	if model == "" {
		model = vertexModelFromBody(req.Body)
	}
	if model == "" {
		return fmt.Errorf("vertex: no model in request")
	}

	url, err := BuildVertexURL(model, req.IsStream, creds)
	if err != nil {
		return err
	}

	geminiBody, err := translator.TranslateOpenAIToGemini(req.Body)
	if err != nil {
		return fmt.Errorf("vertex translate to Gemini: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(geminiBody))
	if err != nil {
		return fmt.Errorf("vertex create request: %w", err)
	}
	for k, v := range BuildVertexHeaders(creds, req.IsStream) {
		httpReq.Header.Set(k, v)
	}

	client := req.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("vertex upstream request: %w", err)
	}
	defer resp.Body.Close()

	forwardUpstreamResponseHeaders(w, resp.Header)

	if resp.StatusCode != http.StatusOK {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, constants.UpstreamErrLimit))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("vertex upstream returned %d and body read failed: %w", resp.StatusCode, readErr)
		}
		return &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: errBody, Header: resp.Header}
	}

	if req.IsStream {
		stallReader := proxy.NewStallReaderWithContext(req.Ctx, resp.Body, 0, "vertex")
		return geminiStream(w, stallReader, req.Ctx)
	}
	return geminiNonStream(w, resp.Body)
}

func vertexModelFromBody(body []byte) string {
	var meta struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Model)
}
