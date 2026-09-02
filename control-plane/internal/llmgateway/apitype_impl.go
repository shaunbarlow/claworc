package llmgateway

import (
	"net/http"
	"regexp"
	"strings"
)

var versionSuffix = regexp.MustCompile(`/v\d+$`)

// pathEndsWithVersion reports whether urlStr's path ends with a versioned
// segment like /v1, /v4, etc.
func pathEndsWithVersion(urlStr string) bool {
	return versionSuffix.MatchString(urlStr)
}

// --- openAICompletions (default / fallback) ---

type openAICompletions struct{}

func (openAICompletions) SetAuthHeader(req *http.Request, mat AuthMaterial) {
	req.Header.Set("Authorization", "Bearer "+mat.APIKey)
}

// RewritePath reconciles the version segment between the provider base URL and
// the incoming request path. OpenClaw's openai-completions client appends
// endpoints straight onto the configured baseUrl (its convention is that the
// baseUrl already carries the version, e.g. https://api.moonshot.ai/v1), and
// Claworc hands it a versionless gateway URL — so the path arrives here as
// /chat/completions. A versionless provider base URL therefore needs /v1
// injected, or the request lands on api.openai.com/chat/completions and gets
// an empty-bodied 404 that looks like an unknown model.
func (openAICompletions) RewritePath(baseURL, requestPath string) string {
	if pathEndsWithVersion(baseURL) && strings.HasPrefix(requestPath, "/v1/") {
		return requestPath[3:]
	}
	if !pathEndsWithVersion(baseURL) && !strings.HasPrefix(requestPath, "/v1/") {
		return "/v1" + requestPath
	}
	return requestPath
}

func (openAICompletions) ParseUsage(body []byte) (int, int, int) {
	return ParseUsageOpenAICompletions(body)
}

func (openAICompletions) ParseStreamingUsage(body []byte) (int, int, int) {
	return ParseUsageOpenAICompletionsStream(body)
}

func (openAICompletions) ProbeURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if pathEndsWithVersion(trimmed) {
		return trimmed + "/models"
	}
	return trimmed + "/v1/models"
}

func (openAICompletions) ProbeHeaders(*http.Request) {}

// --- openAIResponses (embeds openAICompletions for shared auth/probe) ---

type openAIResponses struct {
	openAICompletions
}

// RewritePath is inherited from the embedded openAICompletions — both types
// need identical /v1 reconciliation against the provider base URL.

func (openAIResponses) ParseUsage(body []byte) (int, int, int) {
	return ParseUsageOpenAIResponses(body)
}

func (openAIResponses) ParseStreamingUsage(body []byte) (int, int, int) {
	return ParseUsageOpenAIResponsesStream(body)
}

// --- openAICodexResponses (ChatGPT subscription endpoint) ---
//
// Used when an LLMProvider authenticates via OAuth against ChatGPT's
// /codex/responses endpoint (https://chatgpt.com/backend-api). The request
// body shape is built by OpenClaw inside the container — the gateway only
// rewrites auth headers and forwards.

type openAICodexResponses struct {
	openAIResponses
}

func (openAICodexResponses) SetAuthHeader(req *http.Request, mat AuthMaterial) {
	req.Header.Set("Authorization", "Bearer "+mat.OAuthAccess)
	if mat.OAuthAccount != "" {
		req.Header.Set("chatgpt-account-id", mat.OAuthAccount)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "pi")
}

func (openAICodexResponses) RewritePath(baseURL, requestPath string) string {
	// OpenClaw is configured with api: "openai-responses" (so pi-ai skips its
	// client-side JWT decode), so it posts to /responses or /v1/responses via
	// the OpenAI SDK. The codex backend expects /codex/responses; translate.
	if requestPath == "/codex/responses" {
		return requestPath
	}
	p := strings.TrimPrefix(requestPath, "/v1")
	if p == "/responses" {
		return "/codex/responses"
	}
	return requestPath
}

// Health probes against ChatGPT's backend require valid OAuth credentials and
// hit a real billable endpoint, so we don't expose a probe URL — TestProviderKey
// short-circuits for OAuth providers.
func (openAICodexResponses) ProbeURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/")
}

// --- anthropicMessages ---

type anthropicMessages struct{}

func (anthropicMessages) SetAuthHeader(req *http.Request, mat AuthMaterial) {
	req.Header.Set("x-api-key", mat.APIKey)
}

func (anthropicMessages) RewritePath(baseURL, requestPath string) string {
	if pathEndsWithVersion(baseURL) && strings.HasPrefix(requestPath, "/v1/") {
		return requestPath[3:]
	}
	return requestPath
}

func (anthropicMessages) ParseUsage(body []byte) (int, int, int) {
	return ParseUsageAnthropicMessages(body)
}

func (anthropicMessages) ParseStreamingUsage(body []byte) (int, int, int) {
	return ParseUsageAnthropicMessagesStream(body)
}

func (anthropicMessages) ProbeURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if pathEndsWithVersion(trimmed) {
		return trimmed + "/models"
	}
	return trimmed + "/v1/models"
}

func (anthropicMessages) ProbeHeaders(req *http.Request) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// --- googleGenerativeAI ---

type googleGenerativeAI struct{}

func (googleGenerativeAI) SetAuthHeader(req *http.Request, mat AuthMaterial) {
	req.Header.Set("x-goog-api-key", mat.APIKey)
}

func (googleGenerativeAI) RewritePath(baseURL, requestPath string) string {
	if pathEndsWithVersion(baseURL) && strings.HasPrefix(requestPath, "/v1/") {
		return requestPath[3:]
	}
	return requestPath
}

func (googleGenerativeAI) ParseUsage(body []byte) (int, int, int) {
	return ParseUsageGoogleGenerativeAI(body)
}

func (googleGenerativeAI) ParseStreamingUsage(body []byte) (int, int, int) {
	return ParseUsageGoogleGenerativeAI(body)
}

func (googleGenerativeAI) ProbeURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if pathEndsWithVersion(trimmed) {
		return trimmed + "/models"
	}
	return trimmed + "/v1/models"
}

func (googleGenerativeAI) ProbeHeaders(*http.Request) {}

// --- ollamaAPI ---

type ollamaAPI struct{}

func (ollamaAPI) SetAuthHeader(req *http.Request, mat AuthMaterial) {
	req.Header.Set("Authorization", "Bearer "+mat.APIKey)
}

func (ollamaAPI) RewritePath(baseURL, requestPath string) string {
	if pathEndsWithVersion(baseURL) && strings.HasPrefix(requestPath, "/v1/") {
		return requestPath[3:]
	}
	return requestPath
}

func (ollamaAPI) ParseUsage(body []byte) (int, int, int) {
	return ParseUsageOllama(body)
}

func (ollamaAPI) ParseStreamingUsage(body []byte) (int, int, int) {
	return ParseUsageOllamaStream(body)
}

func (ollamaAPI) ProbeURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/api/tags"
}

func (ollamaAPI) ProbeHeaders(*http.Request) {}

// --- bedrockConverse ---

type bedrockConverse struct{}

func (bedrockConverse) SetAuthHeader(req *http.Request, mat AuthMaterial) {
	req.Header.Set("Authorization", "Bearer "+mat.APIKey)
}

func (bedrockConverse) RewritePath(baseURL, requestPath string) string {
	if pathEndsWithVersion(baseURL) && strings.HasPrefix(requestPath, "/v1/") {
		return requestPath[3:]
	}
	return requestPath
}

func (bedrockConverse) ParseUsage(body []byte) (int, int, int) {
	return ParseUsageBedrockConverseStream(body)
}

func (bedrockConverse) ParseStreamingUsage(body []byte) (int, int, int) {
	return ParseUsageBedrockConverseStream(body)
}

func (bedrockConverse) ProbeURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/")
}

func (bedrockConverse) ProbeHeaders(*http.Request) {}
