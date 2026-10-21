package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// PortsTool is the model-facing name of the TCP port scanner. It only shapes
// arguments: the wall, the budget and the scan itself live in harness/scan.
type PortsTool struct{}

func (p *PortsTool) Name() string { return "ports" }

func (p *PortsTool) Description() string {
	return "Map the TCP ports of one host. Every connect goes through the same egress wall a web request does, so the host's own machine and the private, loopback and cloud metadata ranges are refused with the address named, and each attempt is charged to the engagement budget. Returns one line per port with its state (open, closed or filtered) and the usual service name for the port, then a summary of the counts. A filtered port timed out and may be open behind a firewall, so it is not the same as a closed one. "
}

func (p *PortsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"host":        map[string]any{"type": "string", "description": "Hostname or IP address to scan"},
			"ports":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 500, "description": "Explicit ports to try. Empty uses the common port list"},
			"top":         map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "How many of the common ports to try when ports is empty, default 100"},
			"timeout":     map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "description": "Per-connect timeout in seconds, default 2"},
			"concurrency": map[string]any{"type": "integer", "minimum": 1, "maximum": 64, "description": "Parallel connects, default 16"},
			"max_ports":   map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "description": "Ceiling on ports attempted"},
		},
		"required": []string{"host"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (p *PortsTool) Translate(args map[string]any) (operation.Spec, error) {
	host := strings.TrimSpace(portsStringArg(args["host"]))
	if host == "" {
		return operation.Spec{}, fmt.Errorf("host is required")
	}
	out := map[string]any{"host": host}
	if ports := portsIntListArg(args["ports"], 500); len(ports) > 0 {
		out["ports"] = ports
	}
	out["top"] = portsIntArg(args["top"], 0)
	out["timeout"] = portsIntArg(args["timeout"], 0)
	out["concurrency"] = portsIntArg(args["concurrency"], 0)
	out["max_ports"] = portsIntArg(args["max_ports"], 0)
	return operation.Spec{Type: "ports", Args: out}, nil
}

func (p *PortsTool) TranslateResult(op operation.Operation) string { return op.Output }

func (p *PortsTool) Run(spec operation.Spec) (string, error) {
	return p.RunContext(context.Background(), spec)
}

func (p *PortsTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	opts := scan.PortsOptions{
		Host:        portsStringArg(spec.Args["host"]),
		Ports:       portsIntListArg(spec.Args["ports"], 500),
		TopN:        portsIntArg(spec.Args["top"], 0),
		Timeout:     portsIntArg(spec.Args["timeout"], 0),
		Concurrency: portsIntArg(spec.Args["concurrency"], 0),
		MaxPorts:    portsIntArg(spec.Args["max_ports"], 0),
	}
	result, err := scan.ScanPortsDetailed(ctx, opts)
	if err != nil {
		return "", err
	}
	return formatPortsResult(result, opts.Host), nil
}

// formatPortsResult renders the scan: a header, one bounded line per port
// and the summary of the counts.
func formatPortsResult(result scan.PortsResult, host string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ports: %s - %d ports attempted, %d open, %d closed, %d filtered\n",
		host, result.Attempted, result.Open, result.Closed, result.Filtered)
	for _, port := range result.Ports {
		fmt.Fprintf(&b, "%d %s", port.Port, port.State)
		if port.Service != "" {
			fmt.Fprintf(&b, " %s", port.Service)
		}
		if port.Error != "" {
			fmt.Fprintf(&b, " (%s)", port.Error)
		}
		b.WriteString("\n")
	}
	if result.Skipped > 0 {
		fmt.Fprintf(&b, "skipped: %d ports left untried by the ceiling\n", result.Skipped)
	}
	fmt.Fprintf(&b, "refused: %d by scope; errors: %d\n", result.Refused, len(result.Errors))
	if result.Stopped != "" {
		fmt.Fprintf(&b, "stopped: %s\n", result.Stopped)
	}
	for _, message := range result.Errors {
		fmt.Fprintf(&b, "error: %s\n", message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// portsStringArg reads a string argument without the zero-value trap.
func portsStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// portsIntArg reads a non-negative integer argument, falling back when the
// value is absent or unusable.
func portsIntArg(v any, fallback int) int {
	switch typed := v.(type) {
	case int:
		if typed >= 0 {
			return typed
		}
	case float64:
		if typed >= 0 && typed == float64(int(typed)) {
			return int(typed)
		}
	}
	return fallback
}

// portsIntListArg reads a list of integers in both the native and the
// JSON-decoded shape.
func portsIntListArg(v any, max int) []int {
	switch typed := v.(type) {
	case []int:
		if len(typed) > max {
			typed = typed[:max]
		}
		return typed
	case []any:
		out := make([]int, 0, len(typed))
		for _, entry := range typed {
			switch number := entry.(type) {
			case int:
				out = append(out, number)
			case float64:
				if number == float64(int(number)) {
					out = append(out, int(number))
				}
			}
		}
		if len(out) > max {
			out = out[:max]
		}
		return out
	}
	return nil
}
