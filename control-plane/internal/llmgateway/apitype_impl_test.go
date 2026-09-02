package llmgateway

import "testing"

// TestUsageAPITypeForPath covers the case a model-level api override creates:
// a provider declared openai-completions receiving /responses traffic. The two
// payloads report usage under disjoint keys, so picking the parser from the
// provider's api type alone records 0 tokens and $0.00 cost.
func TestUsageAPITypeForPath(t *testing.T) {
	responsesBody := []byte(`{"usage":{"input_tokens":8,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}}}`)
	completionsBody := []byte(`{"usage":{"prompt_tokens":8,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`)
	// Real /v1/embeddings usage payload: completions-style, no cached details.
	embeddingsBody := []byte(`{"object":"list","data":[],"usage":{"prompt_tokens":2,"total_tokens":2}}`)

	cases := []struct {
		name        string
		apiType     string
		requestPath string
		body        []byte
		wantIn      int
		wantOut     int
		wantCached  int
	}{
		{
			"completions provider, /v1/responses path — parses responses usage",
			"openai-completions", "/v1/responses", responsesBody, 8, 5, 2,
		},
		{
			"completions provider, versionless /responses path",
			"openai-completions", "/responses", responsesBody, 8, 5, 2,
		},
		{
			"completions provider, /chat/completions path — unchanged",
			"openai-completions", "/v1/chat/completions", completionsBody, 6, 5, 2,
		},
		{
			"responses provider, /chat/completions path — parses completions usage",
			"openai-responses", "/v1/chat/completions", completionsBody, 6, 5, 2,
		},
		{
			// Embeddings ride whichever provider the agent's embedding model
			// resolves to and report usage completions-style, so they must not be
			// parsed as responses even on a responses-declared provider.
			"responses provider, /v1/embeddings path — parses completions usage",
			"openai-responses", "/v1/embeddings", embeddingsBody, 2, 0, 0,
		},
		{
			"completions provider, /v1/embeddings path",
			"openai-completions", "/v1/embeddings", embeddingsBody, 2, 0, 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := usageAPITypeForPath(tc.apiType, GetAPIType(tc.apiType), tc.requestPath)
			in, out, cached := at.ParseUsage(tc.body)
			if in != tc.wantIn || out != tc.wantOut || cached != tc.wantCached {
				t.Errorf("got in=%d out=%d cached=%d, want in=%d out=%d cached=%d",
					in, out, cached, tc.wantIn, tc.wantOut, tc.wantCached)
			}
		})
	}
}

// Non-OpenAI api types must keep the APIType they were resolved with, even on a
// path that happens to end in /responses.
func TestUsageAPITypeForPath_LeavesOtherAPITypesAlone(t *testing.T) {
	for _, apiType := range []string{
		"anthropic-messages", "google-generative-ai", "ollama",
		"bedrock-converse", APITypeOpenAICodexResponses,
	} {
		t.Run(apiType, func(t *testing.T) {
			want := GetAPIType(apiType)
			got := usageAPITypeForPath(apiType, want, "/v1/responses")
			if got != want {
				t.Errorf("api type %s should be untouched, got %T", apiType, got)
			}
		})
	}
}
