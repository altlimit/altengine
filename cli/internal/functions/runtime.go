// The JavaScript runtime: turn a deployed ES module into a callable handler.
//
// The deployed artifact is an ES module (`export default { async fetch(request, env) }`),
// and the interpreter has no module system at all. esbuild — already a dependency, since
// the CLI bundles with it — converts the module to CommonJS, which gives us a plain
// script that assigns to `module.exports`. So the same bytes that deploy to the hosted
// service run here, with no second build step and no source rewriting of our own.
//
// Everything the sandbox can see is built here and nowhere else: a function gets exactly
// the globals below plus the `env` it was granted. That is the whole point of the
// emulator — if it works here with these globals, it works there.

package functions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	esbuild "github.com/evanw/esbuild/pkg/api"
)

// How long one invocation may run before it is interrupted.
//
// This is a WALL-CLOCK timeout, not the hosted `cpuMs`. It exists so a runaway loop in a
// local function does not wedge the emulator — not to predict production behaviour. Worth
// knowing: hosted, `cpuMs` is advisory — the platform enforces its own ceiling rather
// than the value we set — so neither number is a guarantee your function will be stopped.
const invocationTimeout = 30 * time.Second

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// compiled is a parsed program, cached so a hot function is not re-transformed per request.
type compiled struct {
	prog *goja.Program
}

type compileCache struct {
	mu    sync.Mutex
	items map[string]*compiled
}

func newCompileCache() *compileCache { return &compileCache{items: map[string]*compiled{}} }

func (c *compileCache) get(key, source string) (*compiled, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it, ok := c.items[key]; ok {
		return it, nil
	}
	// ESM -> CJS. Format is the only thing that matters here; the source is already
	// bundled, so there is nothing to resolve.
	res := esbuild.Transform(source, esbuild.TransformOptions{
		Format:   esbuild.FormatCommonJS,
		Target:   esbuild.ES2020, // the interpreter's ceiling, below the hosted target
		Loader:   esbuild.LoaderJS,
		LogLevel: esbuild.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		m := res.Errors[0]
		where := ""
		if m.Location != nil {
			where = fmt.Sprintf(" (line %d)", m.Location.Line)
		}
		return nil, fmt.Errorf("could not parse function%s: %s", where, m.Text)
	}
	prog, err := goja.Compile(key, string(res.Code), true)
	if err != nil {
		return nil, fmt.Errorf("could not compile function: %w", err)
	}
	it := &compiled{prog: prog}
	c.items[key] = it
	return it, nil
}

func (c *compileCache) drop(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
		}
	}
}

// invocation is everything one call needs.
type invocation struct {
	instanceName string
	fn           *Function
	cfg          *Config
	source       string
	cacheKey     string
	req          *http.Request
	body         []byte
	// bindings dispatches env.<service>.<method>(...) calls.
	bindings *bindings
	logf     func(level, msg string)
}

// run executes one request against the function and returns its Response.
func (h *Handler) run(in *invocation) (status int, header http.Header, body []byte, err error) {
	prog, cerr := h.cache.get(in.cacheKey, in.source)
	if cerr != nil {
		return 0, nil, nil, cerr
	}

	vm := goja.New()
	// Field names come back as declared rather than Go-cased: `doc.fieldName`, not
	// `doc.FieldName`. Without this every payload crossing the boundary changes shape.
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	if err := installGlobals(vm, in); err != nil {
		return 0, nil, nil, err
	}

	// A runaway loop must not wedge the emulator. Interrupt is the only lever an
	// embedded interpreter gives us; it unwinds as a JS exception.
	ctx, cancel := context.WithTimeout(context.Background(), invocationTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				vm.Interrupt(fmt.Sprintf("function exceeded %s", invocationTimeout))
			}
		case <-done:
		}
	}()
	defer close(done)

	module := vm.NewObject()
	exports := vm.NewObject()
	_ = module.Set("exports", exports)
	_ = vm.Set("module", module)
	_ = vm.Set("exports", exports)

	if _, err := vm.RunProgram(prog.prog); err != nil {
		return 0, nil, nil, jsError(err)
	}

	handler, herr := defaultFetch(vm, module)
	if herr != nil {
		return 0, nil, nil, herr
	}

	reqObj, err := newRequestObject(vm, in.req, in.body)
	if err != nil {
		return 0, nil, nil, err
	}
	envObj, err := buildEnv(vm, in)
	if err != nil {
		return 0, nil, nil, err
	}

	res, err := handler(goja.Undefined(), reqObj, envObj)
	if err != nil {
		return 0, nil, nil, jsError(err)
	}
	// A handler may return a promise. The interpreter runs the job queue synchronously,
	// so by the time we look the promise has settled unless it is waiting on something
	// that will never arrive.
	settled, err := resolve(vm, res)
	if err != nil {
		return 0, nil, nil, err
	}
	return readResponse(vm, settled)
}

// defaultFetch pulls `module.exports.default.fetch` out, with an error that names the
// actual shape a function must export — the most common first-run mistake.
func defaultFetch(vm *goja.Runtime, module *goja.Object) (goja.Callable, error) {
	exports := module.Get("exports")
	obj, ok := exports.(*goja.Object)
	if !ok {
		return nil, fmt.Errorf("function exports nothing; expected `export default { async fetch(request, env) {} }`")
	}
	def := obj.Get("default")
	defObj, ok := def.(*goja.Object)
	if !ok || def == nil {
		return nil, fmt.Errorf("function has no default export; expected `export default { async fetch(request, env) {} }`")
	}
	fetch, ok := goja.AssertFunction(defObj.Get("fetch"))
	if !ok {
		return nil, fmt.Errorf("default export has no `fetch` method; expected `export default { async fetch(request, env) {} }`")
	}
	return fetch, nil
}

// resolve unwraps a promise if the handler returned one.
func resolve(vm *goja.Runtime, v goja.Value) (goja.Value, error) {
	p, ok := v.Export().(*goja.Promise)
	if !ok {
		return v, nil
	}
	switch p.State() {
	case goja.PromiseStateFulfilled:
		return p.Result(), nil
	case goja.PromiseStateRejected:
		return nil, fmt.Errorf("%s", describe(vm, p.Result()))
	default:
		// Pending after the queue drained means it is waiting on something that never
		// completes — a real await on an unresolved promise, not slowness.
		return nil, fmt.Errorf("function never returned a response (a promise is still pending)")
	}
}

func describe(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "undefined"
	}
	if o, ok := v.(*goja.Object); ok {
		if st := o.Get("stack"); st != nil && !goja.IsUndefined(st) {
			return st.String()
		}
		if msg := o.Get("message"); msg != nil && !goja.IsUndefined(msg) {
			return msg.String()
		}
	}
	return v.String()
}

// jsError unwraps the interpreter's error types into something a developer can read:
// a JS exception becomes its stack, and an interrupt becomes the timeout message rather
// than an opaque "interrupted".
func jsError(err error) error {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return fmt.Errorf("%s", ex.String())
	}
	var it *goja.InterruptedError
	if errors.As(err, &it) {
		return fmt.Errorf("%v", it.Value())
	}
	return err
}

// readResponse turns whatever the handler returned into an HTTP response.
func readResponse(vm *goja.Runtime, v goja.Value) (int, http.Header, []byte, error) {
	obj, ok := v.(*goja.Object)
	if !ok {
		return 0, nil, nil, fmt.Errorf("function returned %s, expected a Response", typeName(v))
	}
	if obj.Get("__isResponse") == nil || !obj.Get("__isResponse").ToBoolean() {
		return 0, nil, nil, fmt.Errorf("function returned an object that is not a Response")
	}
	status := 200
	if s := obj.Get("status"); s != nil && !goja.IsUndefined(s) {
		status = int(s.ToInteger())
	}
	header := http.Header{}
	if hv := obj.Get("headers"); hv != nil {
		if ho, ok := hv.(*goja.Object); ok {
			if raw, ok := ho.Get("__map").Export().(map[string]any); ok {
				for k, val := range raw {
					header.Set(k, fmt.Sprint(val))
				}
			}
		}
	}
	body := ""
	if b := obj.Get("__body"); b != nil && !goja.IsUndefined(b) && !goja.IsNull(b) {
		body = b.String()
	}
	return status, header, []byte(body), nil
}

func typeName(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	return v.ExportType().String()
}

// logLine renders one console argument list the way a browser console would flatten it.
func logLine(vm *goja.Runtime, args []goja.Value) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a == nil || goja.IsUndefined(a) {
			parts = append(parts, "undefined")
			continue
		}
		if goja.IsNull(a) {
			parts = append(parts, "null")
			continue
		}
		if o, ok := a.(*goja.Object); ok {
			if b, err := json.Marshal(o.Export()); err == nil {
				parts = append(parts, string(b))
				continue
			}
		}
		parts = append(parts, a.String())
	}
	return strings.Join(parts, " ")
}

// ErrBodyTooLarge is returned by readBody when the request exceeds MaxRequestBytes.
var ErrBodyTooLarge = errors.New("request body too large")

// readBody reads a deployed function's request body, refusing one that is too big.
//
// It used to be io.ReadAll(io.LimitReader(r.Body, MaxCodeBytes)): the wrong limit — the
// DEPLOY code size, which has nothing to do with a request — and, worse, silently applied.
// A 1.5 MiB upload arrived as exactly 1 MiB of valid JSON with its tail missing, so the
// function answered "body must be JSON" and no layer anywhere said "too large". One extra
// byte is read so exceeding the limit is detectable rather than indistinguishable from a
// body that happens to end there.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxRequestBytes {
		return nil, ErrBodyTooLarge
	}
	return b, nil
}

var _ = log.Printf
