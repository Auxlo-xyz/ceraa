package llm

import "strings"

// QoderBaseURL is the encrypted inference host. api1.qoder.sh appears nowhere
// in the vendor's client: inference goes to api2.qoder.sh
// (/algo/api/v2/service/pro/sse/agent_chat_generation), whose URL, headers and
// body are all minted per request by the signing module (harness/qodersign).
// The public OpenAI-compatible host api2-v2.qoder.sh is a different service
// with different model names and is not interchangeable (it 404s this path).
// The auth and centre hosts are separate and live with the auth package.
const QoderBaseURL = "https://api2.qoder.sh"

// QoderSubscription identifies a Qoder subscription profile.
const QoderSubscription = "qoder"

// DefaultQoderModel is the stronger of the two models the account offers.
const DefaultQoderModel = "Qwen3.8-Max"

// QoderModel describes one directory entry: the key the encrypted endpoint
// routes on, the window the client budgets, and the thinking support.
//
// Display names are what the picker shows; keys are what the wire needs.
// Established from the vendor client's own fallback table, not guessed.
type QoderModel struct {
	// Key is the directory key (qfmodel), the only name the endpoint accepts.
	Key string
	// Display is the picker name (Qwen3.8-Flash).
	Display string
	// ContextWindow is the top tier the client budgets.
	ContextWindow int
	// Thinking mirrors the directory is_reasoning flag.
	Thinking bool
	// Efforts are the selectable thinking levels; empty means none.
	Efforts []string
	// Image mirrors the directory is_vl flag.
	Image bool
}

// QoderModelTable is the intl directory. CN has its own table (qoder.cn hosts,
// different ids); sending an intl key at a CN host 404s, so the two must never
// share this list.
var QoderModelTable = []QoderModel{
	{Key: "auto", Display: "Auto", ContextWindow: 200000, Image: true},
	{Key: "ultimate", Display: "Ultimate", ContextWindow: 1000000, Thinking: true, Efforts: []string{"xhigh", "high", "low", "max", "medium"}, Image: true},
	{Key: "performance", Display: "Performance", ContextWindow: 1000000, Efforts: []string{"xhigh", "high", "low", "max", "medium"}, Image: true},
	{Key: "efficient", Display: "Efficient", ContextWindow: 1000000, Image: true},
	{Key: "smodel", Display: "Sonus", ContextWindow: 1000000, Thinking: true, Efforts: []string{"xhigh", "high", "low", "max", "medium"}, Image: true},
	{Key: "cmodel", Display: "Cantus", ContextWindow: 1000000, Thinking: true, Efforts: []string{"xhigh", "high", "low", "max", "medium"}, Image: true},
	{Key: "qmodel_38max", Display: "Qwen3.8-Max", ContextWindow: 1000000, Thinking: true, Efforts: []string{"xhigh", "low", "medium"}, Image: true},
	{Key: "qfmodel", Display: "Qwen3.8-Flash", ContextWindow: 1000000, Thinking: true, Efforts: []string{"xhigh", "low", "medium"}, Image: true},
	{Key: "qmodel_latest", Display: "Qwen3.7-Max", ContextWindow: 1000000, Image: true},
	{Key: "qmodel", Display: "Qwen3.7-Plus", ContextWindow: 1000000, Image: true},
	{Key: "kmodel_latest", Display: "Kimi-K3", ContextWindow: 1000000, Efforts: []string{"high", "low", "max"}, Image: true},
	{Key: "kmodel", Display: "Kimi-K2.8-Preview", ContextWindow: 1000000, Efforts: []string{"high", "low", "max"}, Image: true},
	{Key: "gmodel", Display: "GLM-5.3", ContextWindow: 1000000, Thinking: true, Efforts: []string{"high", "low", "max"}, Image: true},
	{Key: "gfmodel", Display: "GLM-5.3-Flash", ContextWindow: 1000000, Thinking: true, Efforts: []string{"high", "max"}, Image: true},
	{Key: "dmodel", Display: "DeepSeek-V4-Pro", ContextWindow: 1000000, Thinking: true, Efforts: []string{"high", "max"}, Image: true},
	{Key: "dfmodel", Display: "DeepSeek-Flash", ContextWindow: 1000000, Thinking: true, Efforts: []string{"high", "max", "low"}, Image: true},
	{Key: "mmodel", Display: "MiniMax-M3", ContextWindow: 1000000, Image: true},
}

// QoderModelFor maps a picker name or a directory key to its directory entry.
// A bare key passes through unchanged, so hand configuration keeps working.
func QoderModelFor(model string) (QoderModel, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, m := range QoderModelTable {
		if strings.ToLower(m.Display) == name || strings.ToLower(m.Key) == name {
			return m, true
		}
	}
	return QoderModel{}, false
}
