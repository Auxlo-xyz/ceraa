package qodersign

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
)

// buildImports wires the ./qoder_auth_wasm_bg.js namespace the module was
// built against. Every entry mirrors the reference glue; the three below it
// historically broke the signer in ways that present as hangs, not errors:
//
//   - prototypesetcall copies the JS array INTO wasm memory. Reversing it
//     starves the signer of randomness and spins forever on zeros.
//   - set dispatches on the target: Map.set(k, v) for header maps,
//     Uint8Array.set(src, offset) for buffers.
//   - getRandomValues failures surface through __wbindgen_export, never as a
//     silent no-op: an unfilled buffer reads as zeros and the signer retries
//     forever.
func buildImports(g *glue, r wazero.Runtime) wazero.HostModuleBuilder {
	b := r.NewHostModuleBuilder(importModule)
	b.NewFunctionBuilder().WithFunc(func(a uint32) { g.heap.take(a) }).Export("__wbindgen_object_drop_ref")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 { return g.heap.push(g.heap.get(a)) }).Export("__wbindgen_object_clone_ref")
	b.NewFunctionBuilder().WithFunc(func(a, e uint32) uint32 {
		g.refreshMem()
		buf, ok := g.mem.Read(a, e)
		if !ok {
			panic("view out of bounds")
		}
		cp := append([]byte(nil), buf...)
		return g.heap.push(&jsU8{data: cp})
	}).Export("__wbindgen_cast_0000000000000001")
	b.NewFunctionBuilder().WithFunc(func(a, e uint32) uint32 {
		return g.heap.push(g.readString(a, e))
	}).Export("__wbindgen_cast_0000000000000002")
	b.NewFunctionBuilder().WithFunc(func(a, e, t uint32) uint32 {
		switch dst := g.heap.get(a).(type) {
		case *jsMap:
			dst.set(jsToString(g.heap.get(e)), jsToString(g.heap.get(t)))
			return 0
		case *jsU8:
			src := g.heap.get(e).(*jsU8)
			var n uint32
			switch o := g.heap.get(t).(type) {
			case uint32:
				n = o
			case int:
				n = uint32(o)
			case float64:
				n = uint32(o)
			default:
				panic(fmt.Sprintf("set offset %T", o))
			}
			copy(dst.data[n:], src.data)
			return 0
		default:
			panic(fmt.Sprintf("set on %T", dst))
		}
	}).Export("__wbg_set_08463b1df38a7e29")
	// getRandomValues writing straight into wasm memory.
	b.NewFunctionBuilder().WithFunc(func(x, y uint32) {
		defer func() {
			if rec := recover(); rec != nil {
				g.callExport("__wbindgen_export", uint64(g.heap.push(fmt.Errorf("%v", rec))))
			}
		}()
		g.refreshMem()
		buf := make([]byte, y)
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		if !g.mem.Write(x, buf) {
			panic("random write out of bounds")
		}
	}).Export("__wbg_getRandomValues_d49329ff89a07af1")
	// getRandomValues on a JS object: fill the target array in place.
	b.NewFunctionBuilder().WithFunc(func(x, y uint32) {
		defer func() {
			if rec := recover(); rec != nil {
				g.callExport("__wbindgen_export", uint64(g.heap.push(fmt.Errorf("%v", rec))))
			}
		}()
		tgt := g.heap.get(y).(*jsU8)
		if _, err := rand.Read(tgt.data); err != nil {
			panic(err)
		}
	}).Export("__wbg_getRandomValues_c44a50d8cfdaebeb")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 {
		return g.heap.push(&jsCrypto{})
	}).Export("__wbg_crypto_38df2bab126b63dc")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 { return g.heap.push(jsUndef{}) }).Export("__wbg_process_44c7a14e11e9f69e")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 { return g.heap.push(jsUndef{}) }).Export("__wbg_versions_276b2795b1c6a219")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 { return g.heap.push(jsUndef{}) }).Export("__wbg_node_84ea875411254db1")
	b.NewFunctionBuilder().WithFunc(func() uint32 {
		return g.heap.push("require-stub")
	}).Export("__wbg_require_b4edbdcf3e2a1ef0")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 { return g.heap.push(jsUndef{}) }).Export("__wbg_msCrypto_bd5a034af96bcba6")
	b.NewFunctionBuilder().WithFunc(func(x, y uint32) {
		defer func() {
			if rec := recover(); rec != nil {
				g.callExport("__wbindgen_export", uint64(g.heap.push(fmt.Errorf("%v", rec))))
			}
		}()
		tgt := g.heap.take(y).(*jsU8)
		if _, err := rand.Read(tgt.data); err != nil {
			panic(err)
		}
	}).Export("__wbg_randomFillSync_6c25eac9869eb53c")
	b.NewFunctionBuilder().WithFunc(func(fn, self, arg uint32) uint32 {
		f, ok := g.heap.get(fn).(jsFunc)
		if !ok {
			panic(fmt.Sprintf("call of non-function %T", g.heap.get(fn)))
		}
		var res uint32
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					g.callExport("__wbindgen_export", uint64(g.heap.push(fmt.Errorf("%v", rec))))
				}
			}()
			res = g.heap.push(f(g.heap.get(self), g.heap.get(arg)))
		}()
		return res
	}).Export("__wbg_call_d578befcc3145dee")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 {
		return g.heap.push(&jsU8{data: make([]byte, a)})
	}).Export("__wbg_new_with_length_9cedd08484b73942")
	b.NewFunctionBuilder().WithFunc(func(a uint32) uint32 {
		return uint32(len(g.heap.get(a).(*jsU8).data))
	}).Export("__wbg_length_0c32cb8543c8e4c8")
	b.NewFunctionBuilder().WithFunc(func(a, e, t uint32) {
		src := g.heap.get(t).(*jsU8)
		if uint32(len(src.data)) > e {
			panic("set: source longer than view")
		}
		g.refreshMem()
		if !g.mem.Write(a, src.data) {
			panic("copy destination out of bounds")
		}
	}).Export("__wbg_prototypesetcall_3e05eb9545565046")
	b.NewFunctionBuilder().WithFunc(func(a, e, t uint32) uint32 {
		src := g.heap.get(a).(*jsU8)
		return g.heap.push(&jsU8{data: src.data[e:t]})
	}).Export("__wbg_subarray_0f98d3fb634508ad")
	b.NewFunctionBuilder().WithFunc(func() uint32 {
		return g.heap.push(&jsMap{vals: map[string]string{}})
	}).Export("__wbg_new_99cabae501c0a8a0")
	b.NewFunctionBuilder().WithFunc(func() float64 {
		return float64(time.Now().UnixMilli())
	}).Export("__wbg_now_88621c9c9a4f3ffc")
	b.NewFunctionBuilder().WithFunc(func() uint32 { return g.heap.push(jsGlobal{}) }).Export("__wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f")
	b.NewFunctionBuilder().WithFunc(func() uint32 { return g.heap.push(jsGlobal{}) }).Export("__wbg_static_accessor_GLOBAL_f2e0f995a21329ff")
	b.NewFunctionBuilder().WithFunc(func() uint32 { return 0 }).Export("__wbg_static_accessor_SELF_24f78b6d23f286ea")
	b.NewFunctionBuilder().WithFunc(func() uint32 { return 0 }).Export("__wbg_static_accessor_WINDOW_59fd959c540fe405")
	b.NewFunctionBuilder().WithFunc(func(p, l uint32) {
		panic(g.readString(p, l))
	}).Export("__wbg___wbindgen_throw_81fc77679af83bc6")
	b.NewFunctionBuilder().WithFunc(func(p, l uint32) uint32 {
		return g.heap.push(fmt.Errorf("%s", g.readString(p, l)))
	}).Export("__wbg_Error_2e59b1b37a9a34c3")
	b.NewFunctionBuilder().WithFunc(func(id uint32) uint32 {
		switch v := g.heap.get(id).(type) {
		case nil, jsUndef:
			return 0
		case jsNull:
			return 1 // typeof null === 'object'
		case string:
			return 0
		case jsFunc:
			return 0
		default:
			_ = v
			return 1
		}
	}).Export("__wbg___wbindgen_is_object_40c5a80572e8f9d3")
	b.NewFunctionBuilder().WithFunc(func(id uint32) uint32 {
		_, ok := g.heap.get(id).(string)
		if ok {
			return 1
		}
		return 0
	}).Export("__wbg___wbindgen_is_string_b29b5c5a8065ba1a")
	b.NewFunctionBuilder().WithFunc(func(id uint32) uint32 {
		_, ok := g.heap.get(id).(jsFunc)
		if ok {
			return 1
		}
		return 0
	}).Export("__wbg___wbindgen_is_function_49868bde5eb1e745")
	b.NewFunctionBuilder().WithFunc(func(id uint32) uint32 {
		switch v := g.heap.get(id).(type) {
		case nil, jsUndef:
			return 1
		default:
			_ = v
			return 0
		}
	}).Export("__wbg___wbindgen_is_undefined_c0cca72b82b86f4d")
	return b
}
