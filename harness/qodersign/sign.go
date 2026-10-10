package qodersign

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
)

// ClientVersion is baked into the signature payload; the server checks it.
const ClientVersion = "1.1.49"

// UserInfo is the identity a signature is bound to. The token is the stored
// access token; the uid is what makes the signature valid, and signing without
// one is refused rather than attempted.
type UserInfo struct {
	UID              string
	Token            string
	OrganizationID   string
	OrganizationTags []string
	DataPolicyAgreed bool
}

// ClientMetadata identifies the calling client inside the encrypted envelope.
// These are the vendor CLI's own values; anything else signs a request the
// server does not recognise as its client.
type ClientMetadata struct {
	ClientType      string
	BusinessProduct string
	BusinessType    string
	Scene           string
}

// DefaultClientMetadata is the CLI identity the reference uses.
func DefaultClientMetadata() ClientMetadata {
	return ClientMetadata{
		ClientType:      "5",
		BusinessProduct: "cli",
		BusinessType:    "agent",
		Scene:           "assistant",
	}
}

// Signer holds one instantiated module. Safe for concurrent use; WASM calls
// are serialised because the underlying context is not thread safe.
type Signer struct {
	mu sync.Mutex
	rt wazero.Runtime
	g  *glue
}

// New instantiates the signer. The module has no start function with side
// effects; instantiation only resolves imports.
func New(ctx context.Context) (*Signer, error) {
	s := &Signer{g: &glue{heap: newObjectHeap()}}
	s.rt = wazero.NewRuntime(ctx)
	host := buildImports(s.g, s.rt)
	if _, err := host.Instantiate(ctx); err != nil {
		s.rt.Close(ctx)
		return nil, fmt.Errorf("qodersign host: %w", err)
	}
	mod, err := s.rt.InstantiateWithConfig(ctx, wasmBytes, wazero.NewModuleConfig())
	if err != nil {
		s.rt.Close(ctx)
		return nil, fmt.Errorf("qodersign instantiate: %w", err)
	}
	s.g.mod = mod
	return s, nil
}

// Close releases the runtime.
func (s *Signer) Close(ctx context.Context) error {
	return s.rt.Close(ctx)
}

// AuthFields returns the encrypt_user_info and key the server uses to
// recognise this identity. Both are ciphertext; a dummy identity still yields
// well-formed fields, so this proves the mechanism, not the account.
func (s *Signer) AuthFields(user UserInfo) (encryptUserInfo, key string, err error) {
	if strings.TrimSpace(user.UID) == "" {
		return "", "", fmt.Errorf("qodersign: signing without a uid always fails; sign in again")
	}
	payload, err := json.Marshal(map[string]any{
		"uid":                  user.UID,
		"security_oauth_token": user.Token,
		"organization_id":      user.OrganizationID,
		"organization_tags":    orEmpty(user.OrganizationTags),
		"data_policy_agreed":   user.DataPolicyAgreed,
	})
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out string
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("qodersign auth fields: %v", rec)
			}
		}()
		out = s.g.callString(func(stack uint32) {
			a := s.g.writeString(string(payload))
			s.g.callExport("generate_runtime_auth_fields", uint64(stack), uint64(a), uint64(s.g.lastLength))
		})
	}()
	if err != nil {
		return "", "", err
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(out), &fields); err != nil {
		return "", "", fmt.Errorf("qodersign auth fields: unreadable reply")
	}
	if fields["encrypt_user_info"] == "" || fields["key"] == "" {
		return "", "", fmt.Errorf("qodersign auth fields: empty reply")
	}
	return fields["encrypt_user_info"], fields["key"], nil
}

// Context is one account's signing context inside the module. Valid while its
// Signer lives; not usable across signers.
type Context struct {
	signer *Signer
	ptr    uint32
}

// NewContext builds the signing context for one account. The version affects
// Cosy-Version and the signature payload; the metadata must be the CLI
// identity or the server routes the request as a stranger.
func (s *Signer) NewContext(machineID, version string, user UserInfo, fields map[string]string, meta ClientMetadata) (ctx *Context, err error) {
	if strings.TrimSpace(user.UID) == "" {
		return nil, fmt.Errorf("qodersign: signing without a uid always fails; sign in again")
	}
	userInfo, err := json.Marshal(map[string]any{
		"uid":                user.UID,
		"encrypt_user_info":  fields["encrypt_user_info"],
		"key":                fields["key"],
		"organization_id":    user.OrganizationID,
		"organization_tags":  orEmpty(user.OrganizationTags),
		"data_policy_agreed": user.DataPolicyAgreed,
	})
	if err != nil {
		return nil, err
	}
	metaJSON, err := json.Marshal(map[string]string{
		"client_type":      meta.ClientType,
		"business_product": meta.BusinessProduct,
		"business_type":    meta.BusinessType,
		"scene":            meta.Scene,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ptr uint32
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("qodersign context: %v", rec)
			}
		}()
		ptr = s.g.callPointer(func(stack uint32) {
			machine := s.g.writeString(machineID)
			machineLen := s.g.lastLength
			ver := s.g.writeString(version)
			verLen := s.g.lastLength
			info := s.g.writeString(string(userInfo))
			infoLen := s.g.lastLength
			meta := s.g.writeString(string(metaJSON))
			metaLen := s.g.lastLength
			s.g.callExport("qodercontext_new", uint64(stack), uint64(machine), uint64(machineLen), uint64(ver), uint64(verLen), uint64(info), uint64(infoLen), uint64(meta), uint64(metaLen))
		})
	}()
	if err != nil {
		return nil, err
	}
	return &Context{signer: s, ptr: ptr}, nil
}

// PreparedRequest is a ready-to-send inference request: the URL, headers and
// body exactly as the signer produced them. The Authorization header must be
// sent verbatim; replacing it with the plain token answers 403.
type PreparedRequest struct {
	URL     string
	Headers map[string]string
	Body    string
}

// PrepareInfer signs one inference request. The payload is the plaintext
// request body built by BuildPayload; modelKey is the directory key (qfmodel,
// not a display name); source defaults to "system" when empty.
func (c *Context) PrepareInfer(host, payloadJSON, modelKey, source string) (req *PreparedRequest, err error) {
	if strings.TrimSpace(source) == "" {
		source = "system"
	}
	s := c.signer
	s.mu.Lock()
	defer s.mu.Unlock()
	var result uint32
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("qodersign prepare: %v", rec)
			}
		}()
		result = s.g.callPointer(func(stack uint32) {
			hostPtr := s.g.writeString(host)
			hostLen := s.g.lastLength
			body := s.g.writeString(payloadJSON)
			bodyLen := s.g.lastLength
			key := s.g.writeString(modelKey)
			keyLen := s.g.lastLength
			src := s.g.writeString(source)
			srcLen := s.g.lastLength
			s.g.callExport("qodercontext_prepareInferRequest", uint64(stack), uint64(c.ptr), uint64(hostPtr), uint64(hostLen), uint64(body), uint64(bodyLen), uint64(key), uint64(keyLen), uint64(src), uint64(srcLen))
		})
	}()
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("qodersign headers: %v", rec)
			}
		}()
		hIdx := uint32(s.g.callExport("requestresult_headers", uint64(result))[0])
		hm, ok := s.g.heap.take(hIdx).(*jsMap)
		if !ok {
			panic("headers are not a map")
		}
		headers = make(map[string]string, len(hm.keys))
		for _, k := range hm.keys {
			headers[k] = hm.vals[k]
		}
	}()
	if err != nil {
		return nil, err
	}
	return &PreparedRequest{
		URL:     s.g.readResultString("requestresult_url", result),
		Headers: headers,
		Body:    s.g.readResultString("requestresult_body", result),
	}, nil
}

func orEmpty(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
