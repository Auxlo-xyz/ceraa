package llm

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
