package llm

// QoderBaseURL is the inference host, from the vendor's own endpoint discovery.
// The auth and centre hosts are separate and live with the auth package.
const QoderBaseURL = "https://api1.qoder.sh"

// QoderSubscription identifies a Qoder subscription profile.
const QoderSubscription = "qoder"

// DefaultQoderModel is the stronger of the two models the account offers.
const DefaultQoderModel = "Qwen3.8-Max"
