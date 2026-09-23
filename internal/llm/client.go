package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type Message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func Chat(api, key, model string, msgs []Message, tools []map[string]any) (Message, error) {
	body := map[string]any{
		"model": model, "messages": msgs, "temperature": 0.2,
	}
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
		return Message{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return Message{}, fmt.Errorf("%d %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Choices []struct{ Message Message `json:"message"` } `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Message{}, err
	}
	if len(out.Choices) == 0 {
		return Message{}, fmt.Errorf("no choices")
	}
	return out.Choices[0].Message, nil
}

func Config() (api, key, model string) {
	api = os.Getenv("SORAA_API_URL")
	if api == "" {
		api = "https://api.atria-asi.ai/v1"
	}
	key = os.Getenv("SORAA_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	model = os.Getenv("SORAA_MODEL")
	if model == "" {
		model = "atria-dawn-preview"
	}
	return
}
