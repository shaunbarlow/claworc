package llmgateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
)

// ParseUsageOpenAICompletions parses token counts from an OpenAI chat/completions response.
func ParseUsageOpenAICompletions(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var u struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &u) == nil {
		cachedInputTokens = u.Usage.PromptTokensDetails.CachedTokens
		inputTokens = u.Usage.PromptTokens - cachedInputTokens
		outputTokens = u.Usage.CompletionTokens
	}
	return
}

// ParseUsageOpenAIResponses parses token counts from an OpenAI responses API response.
// The responses API uses input_tokens/output_tokens with cached tokens under input_tokens_details.
func ParseUsageOpenAIResponses(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var u struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &u) == nil {
		inputTokens = u.Usage.InputTokens
		outputTokens = u.Usage.OutputTokens
		cachedInputTokens = u.Usage.InputTokensDetails.CachedTokens
	}
	return
}

// ParseUsageAnthropicMessages parses token counts from an Anthropic messages response.
func ParseUsageAnthropicMessages(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var u struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &u) == nil {
		inputTokens = u.Usage.InputTokens
		outputTokens = u.Usage.OutputTokens
		cachedInputTokens = u.Usage.CacheReadInputTokens
	}
	return
}

// ParseUsageGoogleGenerativeAI parses token counts from a Google Generative AI response.
func ParseUsageGoogleGenerativeAI(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var u struct {
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(body, &u) == nil {
		inputTokens = u.UsageMetadata.PromptTokenCount
		outputTokens = u.UsageMetadata.CandidatesTokenCount
		cachedInputTokens = u.UsageMetadata.CachedContentTokenCount
	}
	return
}

// ParseUsageOpenAICompletionsStream extracts token counts from a buffered OpenAI SSE stream.
// Token counts are only present when the request included stream_options: {include_usage: true},
// which causes a final chunk (before [DONE]) to carry the usage object. Without that option the
// stream contains no usage data and all counts will be zero.
func ParseUsageOpenAICompletionsStream(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var chunk struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage.PromptTokens > 0 {
			cachedInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			inputTokens = chunk.Usage.PromptTokens - cachedInputTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
	}
	return
}

const maxSSEEventDataBytes = 16 << 20 // 16 MiB

// forEachSSEEvent reads complete SSE frames without bufio.Scanner's 64 KiB
// token limit. SSE permits an event's JSON payload to span multiple data:
// fields; their values are joined with newlines before being decoded.
func forEachSSEEvent(body []byte, fn func(eventName string, data []byte) bool) error {
	reader := bufio.NewReader(bytes.NewReader(body))
	var eventName string
	var data bytes.Buffer

	dispatch := func() bool {
		if data.Len() == 0 {
			eventName = ""
			return true
		}
		payload := data.Bytes()
		// SSE appends a newline after every data field and removes the final one
		// when dispatching the event.
		payload = payload[:len(payload)-1]
		keepGoing := fn(eventName, payload)
		eventName = ""
		data.Reset()
		return keepGoing
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			switch {
			case line == "":
				if !dispatch() {
					return nil
				}
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				value := strings.TrimPrefix(line, "data:")
				if strings.HasPrefix(value, " ") {
					value = value[1:]
				}
				if data.Len()+len(value)+1 > maxSSEEventDataBytes {
					return fmt.Errorf("SSE event data exceeds %d bytes", maxSSEEventDataBytes)
				}
				data.WriteString(value)
				data.WriteByte('\n')
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			// Be liberal with upstreams that omit the final blank line.
			dispatch()
			return nil
		}
	}
}

// ParseUsageOpenAIResponsesStream extracts token counts from a buffered OpenAI Responses API SSE stream.
// Token counts are carried in response.completed (native OpenAI), response.done
// (ChatGPT/Codex), or response.incomplete terminal events under response.usage.
func ParseUsageOpenAIResponsesStream(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	type responseUsage struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	isTerminal := func(eventType string) bool {
		switch eventType {
		case "response.completed", "response.done", "response.incomplete":
			return true
		default:
			return false
		}
	}

	sawTerminal := false
	err := forEachSSEEvent(body, func(eventName string, data []byte) bool {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return true
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Usage *responseUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			if isTerminal(eventName) {
				log.Printf("[gateway] OpenAI Responses usage parser could not decode terminal SSE event type=%s bytes=%d", safeLog(eventName), len(data))
			}
			return true
		}
		if !isTerminal(event.Type) {
			return true
		}
		sawTerminal = true
		if event.Response.Usage == nil {
			log.Printf("[gateway] OpenAI Responses terminal SSE event has no usage type=%s bytes=%d", safeLog(event.Type), len(data))
			return false
		}
		inputTokens = event.Response.Usage.InputTokens
		outputTokens = event.Response.Usage.OutputTokens
		cachedInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
		return false
	})
	if err != nil {
		log.Printf("[gateway] OpenAI Responses usage parser failed captured_bytes=%d error=%v", len(body), err)
	} else if !sawTerminal {
		log.Printf("[gateway] OpenAI Responses usage parser found no terminal SSE event captured_bytes=%d", len(body))
	}
	return
}

// ParseUsageAnthropicMessagesStream extracts token counts from a buffered Anthropic SSE stream.
// Input tokens and cache counts come from the message_start event; final output token count
// comes from the message_delta event (which supersedes the preliminary count in message_start).
func ParseUsageAnthropicMessagesStream(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	type startUsage struct {
		InputTokens          int `json:"input_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
		OutputTokens         int `json:"output_tokens"`
	}
	type deltaUsage struct {
		OutputTokens int `json:"output_tokens"`
	}
	var msgStart struct {
		Type    string `json:"type"`
		Message struct {
			Usage startUsage `json:"usage"`
		} `json:"message"`
	}
	var msgDelta struct {
		Type  string     `json:"type"`
		Usage deltaUsage `json:"usage"`
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if strings.HasPrefix(data, `{"type":"message_start"`) {
			if json.Unmarshal([]byte(data), &msgStart) == nil {
				inputTokens = msgStart.Message.Usage.InputTokens
				cachedInputTokens = msgStart.Message.Usage.CacheReadInputTokens
			}
		} else if strings.HasPrefix(data, `{"type":"message_delta"`) {
			if json.Unmarshal([]byte(data), &msgDelta) == nil {
				outputTokens = msgDelta.Usage.OutputTokens
			}
		}
	}
	return
}

// ParseUsageOllama parses token counts from an Ollama non-streaming response.
// Token counts are top-level fields: prompt_eval_count (input) and eval_count (output).
// Ollama does not report cached tokens.
func ParseUsageOllama(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var r struct {
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if json.Unmarshal(body, &r) == nil {
		inputTokens = r.PromptEvalCount
		outputTokens = r.EvalCount
	}
	return
}

// ParseUsageOllamaStream parses token counts from an Ollama streaming response.
// The stream is newline-delimited JSON (not SSE). Only the final object (done: true)
// carries token counts.
func ParseUsageOllamaStream(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var r struct {
		Done            bool `json:"done"`
		PromptEvalCount int  `json:"prompt_eval_count"`
		EvalCount       int  `json:"eval_count"`
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if json.Unmarshal([]byte(line), &r) == nil && r.Done {
			inputTokens = r.PromptEvalCount
			outputTokens = r.EvalCount
			return
		}
	}
	return
}

// ParseUsageBedrockConverseStream parses token counts from an AWS Bedrock Converse stream
// metadata event. The final event carries usage under metadata.usage with camelCase field names.
// Bedrock does not report cached tokens in this event.
func ParseUsageBedrockConverseStream(body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	var r struct {
		Metadata struct {
			Usage struct {
				InputTokens  int `json:"inputTokens"`
				OutputTokens int `json:"outputTokens"`
			} `json:"usage"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &r) == nil {
		inputTokens = r.Metadata.Usage.InputTokens
		outputTokens = r.Metadata.Usage.OutputTokens
	}
	return
}

// parseProxyUsage delegates to the correct streaming or non-streaming parser via the APIType interface.
func parseProxyUsage(body []byte, at APIType, isStreaming bool) (inputTokens, outputTokens, cachedInputTokens int) {
	if isStreaming {
		return at.ParseStreamingUsage(body)
	}
	return at.ParseUsage(body)
}

// ParseUsage dispatches to the correct parser based on apiType string.
// Public backward-compat wrapper used by external callers.
func ParseUsage(apiType string, body []byte) (inputTokens, outputTokens, cachedInputTokens int) {
	return GetAPIType(apiType).ParseUsage(body)
}
