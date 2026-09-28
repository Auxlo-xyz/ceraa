package tool

import (
	"fmt"
	"strings"

	"ceraa/harness/operation"
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

// SetProviderKeyTool gives a provider the credential held in a stored secret.
//
// This is what "use a secret" usually means for Ceraa, and it adds no new exposure
// surface: the value goes where the provider transport already reads it.
type SetProviderKeyTool struct{}

func (t *SetProviderKeyTool) Name() string { return "set_provider_key" }
func (t *SetProviderKeyTool) Description() string {
	return "Give a provider the credential held in a stored secret, without revealing it. " +
		"Reports the provider, the secret name and the length — never the value. " +
		"Fails for a name that is not stored, or one reserved for Ceraa's own provider " +
		"credentials."
}
func (t *SetProviderKeyTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider id to give the credential to, for example custom or cline"},
			"secret":   map[string]any{"type": "string", "description": "Name of the stored secret holding the credential"},
		},
		"required": []string{"provider", "secret"},
	}
}

func (t *SetProviderKeyTool) Translate(args map[string]any) (operation.Spec, error) {
	provider, _ := args["provider"].(string)
	name, _ := args["secret"].(string)
	if strings.TrimSpace(provider) == "" {
		return operation.Spec{}, fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(name) == "" {
		return operation.Spec{}, fmt.Errorf("secret is required")
	}
	// Name only. See the file comment: a value here would be persisted.
	return operation.Spec{Type: "set_provider_key", Args: map[string]any{
		"provider": strings.TrimSpace(provider), "secret": strings.TrimSpace(name),
	}}, nil
}
func (t *SetProviderKeyTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *SetProviderKeyTool) Sequential() bool                              { return true }
func (t *SetProviderKeyTool) MutationKey(args map[string]any) string {
	provider, _ := args["provider"].(string)
	return "provider:" + provider
}

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
		"The value is not placed in the command line and is not returned; anything the " +
		"command prints has it scrubbed. Fails for an unknown or reserved secret name."
}
func (t *RunCommandWithSecretTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Command to run"},
			"secret":  map[string]any{"type": "string", "description": "Name of the stored secret to inject"},
			"env_var": map[string]any{"type": "string", "description": "Environment variable name to inject it as"},
			"workdir": map[string]any{"type": "string", "description": "Directory to run in"},
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
	}}, nil
}
func (t *RunCommandWithSecretTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *RunCommandWithSecretTool) Sequential() bool                              { return true }
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
