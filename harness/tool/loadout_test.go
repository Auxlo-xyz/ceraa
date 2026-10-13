package tool

import (
	"encoding/json"
	"testing"

	"ceraa/harness/llm"
)

func TestOfferedToolCatalogCostExcludesHiddenReplayTools(t *testing.T) {
	reg := DefaultRegistry()
	all := toolDefinitions(reg.All())
	offered := toolDefinitions(reg.Offered())
	if len(all) <= len(offered) {
		t.Fatalf("all=%d offered=%d, hidden tools were not excluded", len(all), len(offered))
	}
	allTokens := llm.EstimateToolDefinitionTokens(all)
	offeredTokens := llm.EstimateToolDefinitionTokens(offered)
	if allTokens <= offeredTokens {
		t.Fatalf("all=%d offered=%d, hidden definitions did not add cost", allTokens, offeredTokens)
	}
	if _, ok := reg.Get("todowrite"); !ok {
		t.Fatal("hidden replay tool is not executable")
	}
	for _, def := range offered {
		if def.Name == "todowrite" || def.Name == "browser" {
			t.Fatalf("hidden replay tool %q leaked into offered catalog", def.Name)
		}
	}
}

func toolDefinitions(tools []Tool) []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(tools))
	for _, item := range tools {
		defs = append(defs, llm.ToolDef{Name: item.Name(), Description: item.Description(), Parameters: item.Schema()})
	}
	return defs
}

func TestOfferedToolDefinitionsAreJSONSerializable(t *testing.T) {
	for _, item := range DefaultRegistry().Offered() {
		if _, err := json.Marshal(llm.ToolDef{Name: item.Name(), Description: item.Description(), Parameters: item.Schema()}); err != nil {
			t.Fatalf("%s definition: %v", item.Name(), err)
		}
	}
}
