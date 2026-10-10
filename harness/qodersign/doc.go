// Package qodersign produces signed Qoder inference requests.
//
// Qoder's real inference endpoint does not accept a plain bearer token: the
// URL, headers and body of every request are produced by the vendor's own
// signing module, shipped as WebAssembly, and the Authorization header it
// mints (Bearer COSY.<payload>.<signature>) cannot be constructed by hand.
// Sending the token directly answers 403 Signature invalid.
//
// The module here is that signer, run in-process through wazero (pure Go, no
// cgo). Its interface was established from the vendor's wasm-bindgen glue
// rather than guessed: the two result layouts, the (stack, ptr) argument order
// on the result readers, the freelist object heap, and the copy directions on
// the buffer imports all had to match exactly, and each mismatch presented as
// a hang rather than an error.
//
// Provenance: qoder-auth-wasm.wasm is Qoder's client signing module,
// redistributed under MIT by Ebony-Vinyl/dsh-our-free-model
// (vendor/channel-pack/src/qoder-auth-wasm.wasm). It is used here as the
// client's own key for its own lock: encrypt requests exactly as the client
// encrypts them, nothing more.
//
// A Signer is bound to one account's identity (uid + token + machine id) and
// is safe for concurrent use; calls are serialised because the underlying
// context is not thread safe.
package qodersign
