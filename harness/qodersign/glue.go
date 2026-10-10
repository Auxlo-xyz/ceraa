package qodersign

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// glue carries WASM memory access, string marshalling, and the result layouts.
//
// Strings cross as (ptr, len) pairs into linear memory, allocated with
// __wbindgen_export2 and released with __wbindgen_export4. Results come back
// through a 16-byte stack slot in one of two layouts:
//
//	callString:  ptr@+0 len@+4 errObj@+8 isErr@+12  (generate_runtime_auth_fields,
//	            model_cache_decrypt)
//	callPointer: ptr@+0 errObj@+4 isErr@+8          (qodercontext_new,
//	            qodercontext_prepareInferRequest)
//
// Mixing the layouts reads a null pointer as a length and vice versa.
// requestresult_url/body use a third shape: ptr/len at stack+0/+4 with no
// error slot, and the stack pointer comes first, contrary to intuition.
type glue struct {
	mod        api.Module
	mem        api.Memory
	heap       *objectHeap
	lastLength uint32
}

func (g *glue) refreshMem() {
	g.mem = g.mod.Memory()
}

func (g *glue) readString(ptr, length uint32) string {
	g.refreshMem()
	b, ok := g.mem.Read(ptr, length)
	if !ok {
		panic("string read out of bounds")
	}
	return string(b)
}

func (g *glue) callExport(name string, args ...uint64) []uint64 {
	fn := g.mod.ExportedFunction(name)
	if fn == nil {
		panic("no export " + name)
	}
	res, err := fn.Call(context.Background(), args...)
	if err != nil {
		panic(name + ": " + err.Error())
	}
	return res
}

// writeString copies text into WASM memory, returning the pointer. The byte
// length is reported through lastLength, mirroring the reference.
func (g *glue) writeString(s string) uint32 {
	b := []byte(s)
	ptr := uint32(g.callExport("__wbindgen_export2", uint64(len(b)), 1)[0])
	g.refreshMem()
	if !g.mem.Write(ptr, b) {
		panic("string write out of bounds")
	}
	g.lastLength = uint32(len(b))
	return ptr
}

func (g *glue) addStack(delta int32) uint32 {
	return uint32(g.callExport("__wbindgen_add_to_stack_pointer", uint64(uint32(delta)))[0])
}

func (g *glue) getI32(off uint32) uint32 {
	g.refreshMem()
	v, ok := g.mem.ReadUint32Le(off)
	if !ok {
		panic("stack read out of bounds")
	}
	return v
}

func (g *glue) callString(invoke func(stack uint32)) string {
	stack := g.addStack(-16)
	var ptr, length uint32
	defer func() {
		g.addStack(16)
		if ptr != 0 {
			g.callExport("__wbindgen_export4", uint64(ptr), uint64(length), 1)
		}
	}()
	invoke(stack)
	ptr = g.getI32(stack)
	length = g.getI32(stack + 4)
	if g.getI32(stack+12) != 0 {
		panic(fmt.Sprintf("wasm error: %v", g.heap.take(g.getI32(stack+8))))
	}
	return g.readString(ptr, length)
}

func (g *glue) callPointer(invoke func(stack uint32)) uint32 {
	stack := g.addStack(-16)
	defer g.addStack(16)
	invoke(stack)
	ptr := g.getI32(stack)
	if g.getI32(stack+8) != 0 {
		panic(fmt.Sprintf("wasm error: %v", g.heap.take(g.getI32(stack+4))))
	}
	return ptr
}

// readResultString reads a (stack, ptr) result: ptr/len at stack+0/+4.
func (g *glue) readResultString(name string, result uint32) string {
	stack := g.addStack(-16)
	defer g.addStack(16)
	g.callExport(name, uint64(stack), uint64(result))
	ptr := g.getI32(stack)
	ln := g.getI32(stack + 4)
	if ptr == 0 {
		return ""
	}
	return g.readString(ptr, ln)
}
