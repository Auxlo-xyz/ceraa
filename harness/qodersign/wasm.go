package qodersign

import (
	_ "embed"
)

//go:embed qoder-auth-wasm.wasm
var wasmBytes []byte

// importModule is the wasm-bindgen import namespace embedded in the module.
// The name must match byte for byte or instantiation fails on a missing
// import rather than on anything informative.
const importModule = "./qoder_auth_wasm_bg.js"
