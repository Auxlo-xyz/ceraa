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
	var acc = map[int]*ToolCall{}
	err := ChatStream(api, key, model, msgs, tools, func(d StreamDelta) {
		final.Content += d.Content
		final.Reasoning += d.Reasoning
		for _, tc := range d.ToolCalls {
			// find by ID or index
			found := -1
			for idx, existing := range acc {
				if existing.ID == tc.ID && tc.ID != "" {
					found = idx
					break
				}
			}
			if found == -1 {
				// use next index
				found = len(acc)
				acc[found] = &ToolCall{}
			}
			c := acc[found]
			if tc.ID != "" {
				c.ID = tc.ID
			}
			if tc.Type != "" {
				c.Type = tc.Type
			}
			if tc.Function.Name != "" {
				c.Function.Name = tc.Function.Name
			}
			// ChatStream already accumulates, so take latest snapshot
			c.Function.Arguments = tc.Function.Arguments
			if c.Type == "" {
				c.Type = "function"
			}
		}
	})
	if err != nil {
		return Message{}, err
	}
	for i := 0; i < len(acc); i++ {
		if c, ok := acc[i]; ok {
			final.ToolCalls = append(final.ToolCalls, *c)
		}
	}
	return final, nil
}

func ChatStream(api, key, model string, msgs []Message, tools []map[string]any, onDelta func(StreamDelta)) error {
	body := map[string]any{"model": model, "messages": msgs, "temperature": 0.2, "stream": true, "stream_options": map[string]any{"include_usage": false}}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	// Accumulate tool calls by index to handle fragmented streaming args
	acc := map[int]*ToolCall{}
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
					Content    *string `json:"content"`
					Reasoning  *string `json:"reasoning_content"`
					Reasoning2 *string `json:"reasoning"`
					ToolCalls  []struct {
						Index int `json:"index"`
						ID    string `json:"id"`
						Type  string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
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
		for _, tc := range d.ToolCalls {
			existing, ok := acc[tc.Index]
			if !ok {
				existing = &ToolCall{ID: tc.ID, Type: tc.Type}
				existing.Function.Name = tc.Function.Name
				acc[tc.Index] = existing
			}
			if tc.ID != "" {
				existing.ID = tc.ID
			}
			if tc.Function.Name != "" {
				existing.Function.Name = tc.Function.Name
			}
			existing.Function.Arguments += tc.Function.Arguments
			delta.ToolCalls = append(delta.ToolCalls, *existing)
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
