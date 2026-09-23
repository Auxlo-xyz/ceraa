package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	Reasoning        string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"-"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type StreamDelta struct {
	Content   string
	Reasoning string
	ToolCalls []ToolCall
	Done      bool
}

func Chat(api, key, model string, msgs []Message, tools []map[string]any) (Message, error) {
	var final Message
	err := ChatStream(api, key, model, msgs, tools, func(d StreamDelta) {
		final.Content += d.Content
		final.Reasoning += d.Reasoning
		if len(d.ToolCalls) > 0 {
			final.ToolCalls = d.ToolCalls
		}
	})
	return final, err
}

func ChatStream(api, key, model string, msgs []Message, tools []map[string]any, onDelta func(StreamDelta)) error {
	body := map[string]any{"model": model, "messages": msgs, "temperature": 0.2, "stream": true}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", api+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%d %s", resp.StatusCode, string(raw))
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			onDelta(StreamDelta{Done: true})
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          *string    `json:"content"`
					Reasoning        *string    `json:"reasoning_content"`
					Reasoning2       *string    `json:"reasoning"`
					ToolCalls        []ToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil || len(ev.Choices) == 0 {
			continue
		}
		d := ev.Choices[0].Delta
		var delta StreamDelta
		if d.Content != nil {
			delta.Content = *d.Content
		}
		if d.Reasoning != nil {
			delta.Reasoning = *d.Reasoning
		} else if d.Reasoning2 != nil {
			delta.Reasoning = *d.Reasoning2
		}
		if len(d.ToolCalls) > 0 {
			delta.ToolCalls = d.ToolCalls
		}
		if delta.Content != "" || delta.Reasoning != "" || len(delta.ToolCalls) > 0 {
			onDelta(delta)
		}
	}
	return sc.Err()
}

func Config() (api, key, model string) {
	api = os.Getenv("SORAA_API_URL")
	if api == "" {
		api = "https://inference.poolside.ai/v1"
	}
	key = os.Getenv("SORAA_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	model = os.Getenv("SORAA_MODEL")
	if model == "" {
		model = "poolside/laguna-s-2.1"
	}
	return
}
