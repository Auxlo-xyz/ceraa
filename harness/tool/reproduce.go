package tool

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/verify"
)

// ReproduceTool re-checks a source-to-sink claim against fresh file reads.
//
// The verdict comes from verify.ReproduceCode, which anchors both endpoints in
// current contents instead of trusting the transcript the claim arrived in.
// A model confirming its own finding from the same transcript is the failure
// this guards against, so the tool reads the files itself at Run time.
type ReproduceTool struct{}

// ReproduceName is the registry name, shared with the intent gate so the two
// cannot drift apart.
const ReproduceName = "reproduce"

func (t *ReproduceTool) Name() string { return ReproduceName }

func (t *ReproduceTool) Description() string {
	return "Independently re-check a source-to-sink vulnerability claim against current file contents and return PROVEN, REFUTED, UNPROVEN, or INCONCLUSIVE with the reason."
}

func (t *ReproduceTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"claim":         map[string]any{"type": "string"},
			"source_path":   map[string]any{"type": "string"},
			"sink_path":     map[string]any{"type": "string"},
			"source_line":   map[string]any{"type": "integer"},
			"sink_line":     map[string]any{"type": "integer"},
			"source_symbol": map[string]any{"type": "string"},
			"sink_symbol":   map[string]any{"type": "string"},
		},
		"required": []string{"claim", "source_path", "sink_path", "source_line", "sink_line", "source_symbol", "sink_symbol"},
	}
}

// Translate pins the seven claim fields into a reproduce spec. Paths stay
// unresolved here because the workdir arrives on the spec after translation,
// and resolving against the process directory instead would make a file the
// model just created look missing.
func (t *ReproduceTool) Translate(args map[string]any) (operation.Spec, error) {
	for key := range args {
		if key == "action" {
			// A model that names an action anyway has guessed, and the guess is
			// more useful as a rejection than as something quietly ignored.
			return operation.Spec{}, fmt.Errorf(
				"%s is a single-action tool and takes no %q argument; drop it", t.Name(), "action")
		}
		if !reproduceFields[key] {
			return operation.Spec{}, fmt.Errorf("%s does not accept %q", t.Name(), key)
		}
	}
	var missing []string
	for _, key := range reproduceRequired {
		if _, present := args[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return operation.Spec{}, fmt.Errorf("%s requires %s", t.Name(), strings.Join(missing, " and "))
	}
	for _, key := range []string{"claim", "source_path", "sink_path", "source_symbol", "sink_symbol"} {
		text, _ := args[key].(string)
		if strings.TrimSpace(text) == "" {
			return operation.Spec{}, fmt.Errorf("%s requires %s", t.Name(), key)
		}
	}
	sourceLine, err := reproduceLineArg("source_line", args["source_line"])
	if err != nil {
		return operation.Spec{}, err
	}
	sinkLine, err := reproduceLineArg("sink_line", args["sink_line"])
	if err != nil {
		return operation.Spec{}, err
	}
	return operation.Spec{Type: "reproduce", Args: map[string]any{
		"claim":         args["claim"],
		"source_path":   args["source_path"],
		"sink_path":     args["sink_path"],
		"source_line":   sourceLine,
		"sink_line":     sinkLine,
		"source_symbol": args["source_symbol"],
		"sink_symbol":   args["sink_symbol"],
	}}, nil
}

func (t *ReproduceTool) TranslateResult(op operation.Operation) string {
	if op.Error != "" && op.Output == "" {
		return "Error: " + op.Error
	}
	return op.Output
}

// Run satisfies tool.Runner. The check is file reads only, so it runs without
// a context of its own.
func (t *ReproduceTool) Run(spec operation.Spec) (string, error) {
	return t.RunContext(context.Background(), spec)
}

// RunContext resolves both paths here, not in Translate, because the workdir
// arrives on the spec after translation and a relative path means "where my
// commands run".
func (t *ReproduceTool) RunContext(_ context.Context, spec operation.Spec) (string, error) {
	workDir, _ := spec.Args["workdir"].(string)
	rawSource, _ := spec.Args["source_path"].(string)
	rawSink, _ := spec.Args["sink_path"].(string)
	sourcePath, err := ResolvePathFrom(workDir, rawSource)
	if err != nil {
		return "", err
	}
	sinkPath, err := ResolvePathFrom(workDir, rawSink)
	if err != nil {
		return "", err
	}
	sourceLines, sourceErr := readReproduceLines(sourcePath)
	sinkLines, sinkErr := readReproduceLines(sinkPath)
	sourceLine, _ := spec.Args["source_line"].(int)
	sinkLine, _ := spec.Args["sink_line"].(int)
	sourceSymbol, _ := spec.Args["source_symbol"].(string)
	sinkSymbol, _ := spec.Args["sink_symbol"].(string)
	// A file that cannot be read is evidence about the check, not a crash: the
	// read errors ride into ReproduceCode, which maps them to INCONCLUSIVE
	// (held, not discarded) rather than failing the call.
	finding := verify.CodeFinding{
		Source: verify.CodeRef{Path: sourcePath, Line: sourceLine, Symbol: sourceSymbol},
		Sink:   verify.CodeRef{Path: sinkPath, Line: sinkLine, Symbol: sinkSymbol},
	}
	decision := verify.ReproduceCode(finding, func(path string) ([]string, error) {
		switch path {
		case sourcePath:
			return sourceLines, sourceErr
		case sinkPath:
			return sinkLines, sinkErr
		}
		return nil, fmt.Errorf("no such file: %s", path)
	})
	return fmt.Sprintf("VERDICT: %s\n%s", decision.Verdict, decision.Reason), nil
}

// reproduceFields is the closed argument set: anything else is a crossed
// argument naming a mistake the model can correct itself from.
var reproduceFields = map[string]bool{
	"claim":         true,
	"source_path":   true,
	"sink_path":     true,
	"source_line":   true,
	"sink_line":     true,
	"source_symbol": true,
	"sink_symbol":   true,
}

var reproduceRequired = []string{"claim", "source_path", "sink_path", "source_line", "sink_line", "source_symbol", "sink_symbol"}

// reproduceLineArg narrows the model's 1-based line to a positive int. JSON
// numbers arrive as float64; a non-integral float would otherwise truncate
// into a line the model never named.
func reproduceLineArg(name string, value any) (int, error) {
	switch typed := value.(type) {
	case float64:
		if n := int(typed); n > 0 && float64(n) == typed {
			return n, nil
		}
	case int:
		if typed > 0 {
			return typed, nil
		}
	}
	return 0, fmt.Errorf("%s is required and must be a positive integer", name)
}

// readReproduceLines reads a file as 1-based lines, matching the read tool's
// numbering so a line the model just read resolves to the same text.
func readReproduceLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), nil
}
