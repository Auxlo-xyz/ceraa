package tool

import (
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/internal/secret"
)

// Secrets are handled by capability, not by value.
//
// The model is given names and never values. Resolution happens inside the
// operation, at the moment the value is genuinely needed, and the value is never
// returned — not in the output, not in an error, not in a length.
//
// The spec therefore carries the *name* and nothing else. That is not tidiness: an
// operation's Spec.Args is persisted to the session as SpecArgs, so a value placed
// in a spec would be written to disk in plain text and reloaded on every later
// request in that session.

// RunCommandWithSecretTool runs a command with a secret in its environment.
//
// The value is in the process environment and nowhere else: not in the command
// line, so it does not appear in `ps`, and not in the model's context. What the
// command prints is scrubbed on the way out by the redaction the operation layer
// already applies, so an `env` or an `echo $VAR` cannot deliver it.
type RunCommandWithSecretTool struct{}

func (t *RunCommandWithSecretTool) Name() string { return "run_command_with_secret" }
func (t *RunCommandWithSecretTool) Description() string {
	return "Run a shell command with a stored secret injected as an environment variable. " +
		"The value is not placed in the command line and is not returned. " +
		"Fails for an unknown or reserved secret name."
}
func (t *RunCommandWithSecretTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"secret":  map[string]any{"type": "string", "description": "Name of the stored secret to inject"},
			"env_var": map[string]any{"type": "string", "description": "Environment variable name to inject it as"},
			"workdir": map[string]any{"type": "string", "description": "Directory to run in"},
			"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (default 900, max 3600)."},
		},
		"required": []string{"command", "secret", "env_var"},
	}
}

func (t *RunCommandWithSecretTool) Translate(args map[string]any) (operation.Spec, error) {
	command, _ := args["command"].(string)
	name, _ := args["secret"].(string)
	envVar, _ := args["env_var"].(string)
	workdir, _ := args["workdir"].(string)
	if strings.TrimSpace(command) == "" {
		return operation.Spec{}, fmt.Errorf("command is required")
	}
	if strings.TrimSpace(name) == "" {
		return operation.Spec{}, fmt.Errorf("secret is required")
	}
	if !validEnvVarName(envVar) {
		return operation.Spec{}, fmt.Errorf("env_var must be an environment variable name, got %q", envVar)
	}
	return operation.Spec{Type: "run_command_with_secret", Args: map[string]any{
		"command": command, "secret": strings.TrimSpace(name), "env_var": envVar, "workdir": workdir,
		// Carried raw, exactly as bash carries it: the schema above lets the
		// coordinator coerce first, and SpecTimeout reads what arrives.
		"timeout": args["timeout"],
	}}, nil
}
func (t *RunCommandWithSecretTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *RunCommandWithSecretTool) Sequential() bool                              { return true }
func (t *RunCommandWithSecretTool) SequentialQueue() string                       { return "shell" }
func (t *RunCommandWithSecretTool) MutationKey(map[string]any) string             { return "secret-run" }

// validEnvVarName keeps the injected name to something a shell can actually use.
//
// Checked here rather than at exec time because a name with a space or a newline
// in it is either ignored by the shell or, worse, splits into a second variable.
func validEnvVarName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ListSecretsTool reports which secret names the model may ask for.
//
// A value can stay unreadable to the model while the name that refers to it is
// ordinary, discoverable information. Without this tool the model has to be told
// a name out of band before run_command_with_secret can be called at all, which
// quietly makes stored secrets usable only by the person who typed them.
//
// Reserved names are absent, and not merely unlisted: ceraa_* and codex_* hold
// Ceraa's own provider credentials, so a model that could enumerate them could
// aim run_command_with_secret at them, and a names-only view would become a way
// to have the operator's provider keys run through a command of the model's
// choosing. secret.ListUser already applies that filter.
type ListSecretsTool struct{}

func (t *ListSecretsTool) Name() string { return "list_secrets" }
func (t *ListSecretsTool) Description() string {
	return "List the names of the stored secrets you can pass to run_command_with_secret, with " +
		"the length of each value. Names reserved for Ceraa's own " +
		"provider credentials are not listed and cannot be used."
}
func (t *ListSecretsTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *ListSecretsTool) Translate(args map[string]any) (operation.Spec, error) {
	return operation.Spec{Type: "list_secrets", Args: map[string]any{}}, nil
}
func (t *ListSecretsTool) TranslateResult(op operation.Operation) string { return op.Output }

// Run reads the store directly instead of going through an operation: listing
// names mutates nothing, and a read has no reason to occupy the operation queue.
func (t *ListSecretsTool) Run(spec operation.Spec) (string, error) {
	names := secret.ListUser()
	if len(names) == 0 {
		return "No user secrets are stored. The person can add one with /setsecret, " +
			"or from the Hub's Secrets menu.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d stored secret(s):", len(names))
	for _, name := range names {
		// A length is enough to tell a truncated paste from a whole one and is
		// useless for reconstructing the value.
		length := "unknown length"
		if value, err := secret.Get(name); err == nil {
			length = fmt.Sprintf("%d chars", len([]rune(value)))
		}
		fmt.Fprintf(&b, "\n- %s (%s)", name, length)
	}
	return b.String(), nil
}
