package memory

// MemoryType is what kind of claim an extraction is. It survives the store
// because the log writer keeps only durable kinds of claim - profile,
// preference, goal and constraint - and the extractor still names the rest so
// the writer can tell task state apart from a durable fact about the person.
type MemoryType string

const (
	// Preference is how the user wants work done.
	Preference MemoryType = "preference"
	// Profile is who the user is and how they work.
	Profile MemoryType = "profile"
	// Instruction is a standing directive, phrased as one.
	Instruction MemoryType = "instruction"
	// Decision is a choice with a reason that still holds.
	Decision MemoryType = "decision"
	// Goal is something the user is working toward.
	Goal MemoryType = "goal"
	// Constraint is a limit that applies until lifted.
	Constraint MemoryType = "constraint"
	// Request is a one-off task, never a durable fact.
	Request MemoryType = "request"
	// Observation is something noticed once, about the state of the world.
	Observation MemoryType = "observation"
	// Error is a mistake worth not repeating.
	Error MemoryType = "error"
	// Context is a fact about the project or codebase.
	Context MemoryType = "context"
)

// Veracity is how an extraction came to be believed: what the user said
// outright, or what the extractor inferred.
type Veracity string

const (
	// Stated is what the conversation gave a clear explicit signal for.
	Stated Veracity = "stated"
	// Inferred is the extractor's read of a conversation that implied it.
	Inferred Veracity = "inferred"
)
