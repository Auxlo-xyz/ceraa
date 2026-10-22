package tool

// toolset.go is the operator's lever on the tool catalogue.
//
// The catalogue is the cached prefix of every request, and every tool in it
// is tokens the model reads on every turn. The recon work added fifteen
// tools, and the honest question is not "are they worth it" but "which of
// them does this session need".
//
// The toolset answers that. The everyday set is the web review; the
// specialists are the internal engagement: a port map, a directory, a
// Kerberos roast. The operator names the set with an environment variable,
// and a hidden tool is still executable, so a run that knows a tool by name
// can still reach it.

import "os"

// ToolsetEnv names the environment variable that selects the toolset.
const ToolsetEnv = "CERAA_TOOLSET"

// ToolsetName is which set of tools the model is offered. It is not called
// Toolset because the function that reads it is, and Go will not carry both
// a type and a function under one name.
type ToolsetName string

const (
	// ToolsetCore is the everyday web review set.
	ToolsetCore ToolsetName = "core"
	// ToolsetFull adds the internal specialists.
	ToolsetFull ToolsetName = "full"
)

// Toolset returns the configured toolset.
//
// The full set is the default, because the full set is the edge: a session
// that has to be configured into the port scanner and the directory reader
// is a session that cannot use them. The switch exists for the sessions
// that want a smaller catalogue, not to gate the capability.
//
// Only "core" selects the smaller set. An unset or unrecognised value means
// the full set, so a typo in a configuration file does not quietly narrow
// what a session can reach.
func Toolset() ToolsetName {
	switch ToolsetName(os.Getenv(ToolsetEnv)) {
	case ToolsetCore:
		return ToolsetCore
	default:
		return ToolsetFull
	}
}

// ToolsetNames are the values the toolset accepts.
func ToolsetNames() []string {
	return []string{string(ToolsetCore), string(ToolsetFull)}
}
