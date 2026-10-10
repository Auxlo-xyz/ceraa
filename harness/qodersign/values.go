package qodersign

import (
	"fmt"
)

// JS value model for the wasm-bindgen object heap.
//
// Non-numeric values cross the boundary as integer indices into a host-side
// table. Indices 0-1023 are permanently undefined; 1024-1027 are the
// undefined/null/true/false sentinels; 1028+ are live objects with freelist
// recycling. A divergence here shifts every later index and sends execution
// down a path that never terminates, so this mirrors the reference exactly.
type jsUndef struct{}
type jsNull struct{}
type jsGlobal struct{}
type jsCrypto struct{}

// jsU8 is a Uint8Array. Slices share backing arrays exactly like subarray
// views do: a view observes later writes to its parent, and a fill through a
// view is visible through the parent. Copying on view creation freezes history
// and hangs the signer.
type jsU8 struct{ data []byte }

// jsMap is a Map with insertion-ordered keys, preserving header order.
type jsMap struct {
	keys []string
	vals map[string]string
}

func (m *jsMap) set(k, v string) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

type jsFunc func(thisArg, arg any) any

// objectHeap is the index table. Deliberately not thread safe: every WASM call
// runs under the Signer's mutex, matching the single-threaded reference.
type objectHeap struct {
	objects   []any
	firstFree int
}

func newObjectHeap() *objectHeap {
	h := &objectHeap{}
	h.objects = make([]any, 1024)
	h.objects = append(h.objects, jsUndef{}, jsNull{}, true, false)
	h.firstFree = len(h.objects)
	return h
}

func (h *objectHeap) get(i uint32) any {
	if int(i) >= len(h.objects) {
		panic(fmt.Sprintf("heap index %d out of range (len %d)", i, len(h.objects)))
	}
	return h.objects[i]
}

func (h *objectHeap) push(v any) uint32 {
	if h.firstFree == len(h.objects) {
		h.objects = append(h.objects, uint32(len(h.objects)+1))
	}
	idx := h.firstFree
	if n, ok := h.objects[idx].(uint32); ok {
		h.firstFree = int(n)
	} else {
		h.firstFree = len(h.objects)
	}
	h.objects[idx] = v
	return uint32(idx)
}

func (h *objectHeap) take(i uint32) any {
	v := h.get(i)
	if i >= 1028 {
		h.objects[i] = uint32(h.firstFree)
		h.firstFree = int(i)
	}
	return v
}

func jsToString(v any) string {
	switch t := v.(type) {
	case nil, jsUndef:
		return "undefined"
	case jsNull:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", t)
	case uint32:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}
