package tests

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/xk6-browser/chromium"
	"github.com/grafana/xk6-browser/common"
	"github.com/grafana/xk6-browser/env"
	"github.com/grafana/xk6-browser/k6ext"
	"github.com/grafana/xk6-browser/k6ext/k6test"

	k6http "go.k6.io/k6/js/modules/k6/http"
	k6httpmultibin "go.k6.io/k6/lib/testutils/httpmultibin"
	k6metrics "go.k6.io/k6/metrics"
)

// testBrowser is a test testBrowser for integration testing.
type testBrowser struct {
	t testing.TB

	ctx      context.Context
	http     *k6httpmultibin.HTTPMultiBin
	vu       *k6test.VU
	logCache *logCache

	pid   int // the browser process ID
	wsURL string

	browserType *chromium.BrowserType

	*common.Browser

	cancel context.CancelFunc
}

type testBrowserOptions struct {
	fileServer   bool
	logCache     bool
	httpMultiBin bool
	samples      chan k6metrics.SampleContainer
	skipClose    bool
	lookupFunc   env.LookupFunc
}

func newTestBrowserOptions(opts ...any) *testBrowserOptions {
	// default lookup function is env.Lookup so that we can
	// pass the environment variables while testing, i.e.: K6_BROWSER_LOG.
	tbo := &testBrowserOptions{
		samples:    make(chan k6metrics.SampleContainer, 1000),
		lookupFunc: env.Lookup,
	}
	for _, opt := range opts {
		switch opt := opt.(type) {
		case httpServerOption:
			tbo.httpMultiBin = true
		case fileServerOption:
			tbo.fileServer = true
			tbo.httpMultiBin = true
		case logCacheOption:
			tbo.logCache = true
		case skipCloseOption:
			tbo.skipClose = true
		case withSamplesListener:
			tbo.samples = opt
		case env.LookupFunc:
			tbo.lookupFunc = opt
		}
	}

	return tbo
}

// newTestBrowser configures and launches a new chrome browser.
//
// It automatically closes it when `t` returns unless `withSkipClose` option is provided.
//
// The following opts are available to customize the testBrowser:
//   - withHTTPServer: enables the HTTPMultiBin server.
//   - withFileServer: enables the HTTPMultiBin server and serves the given files.
//   - withLogCache: enables the log cache.
//   - withSamplesListener: provides a channel to receive the browser metrics.
//   - env.LookupFunc: provides a custom lookup function for environment variables.
//   - withSkipClose: skips closing the browser when the test finishes.
func newTestBrowser(tb testing.TB, opts ...any) *testBrowser {
	tb.Helper()

	tbopts := newTestBrowserOptions(opts...)
	bt, vu, stop := newBrowserTypeWithVU(tb, tbopts)
	tb.Cleanup(stop)

	// enable the HTTP test server only when necessary
	var (
		testServer *k6httpmultibin.HTTPMultiBin
		state      = vu.StateField
		lc         *logCache
	)

	if tbopts.logCache {
		lc = attachLogCache(tb, state.Logger)
	}
	if tbopts.httpMultiBin {
		testServer = k6httpmultibin.NewHTTPMultiBin(tb)
		state.TLSConfig = testServer.TLSClientConfig
		state.Transport = testServer.HTTPTransport
	}

	b, pid, err := bt.Launch(vu.Context())
	if err != nil {
		tb.Fatalf("testBrowser: %v", err)
	}
	cb, ok := b.(*common.Browser)
	if !ok {
		tb.Fatalf("testBrowser: unexpected browser %T", b)
	}

	tb.Cleanup(func() {
		select {
		case <-vu.Context().Done():
		default:
			if !tbopts.skipClose {
				b.Close()
			}
		}
	})

	tbr := &testBrowser{
		t:           tb,
		ctx:         bt.Ctx,
		http:        testServer,
		vu:          vu,
		logCache:    lc,
		Browser:     cb,
		browserType: bt,
		pid:         pid,
		wsURL:       cb.WsURL(),
		cancel:      stop,
	}
	if tbopts.fileServer {
		tbr = tbr.withFileServer()
	}

	return tbr
}

// NewPage is a wrapper around Browser.NewPage that fails the test if an
// error occurs. Added this helper to avoid boilerplate code in tests.
func (b *testBrowser) NewPage(opts goja.Value) *common.Page {
	b.t.Helper()

	p, err := b.Browser.NewPage(opts)
	require.NoError(b.t, err)

	pp, ok := p.(*common.Page)
	require.Truef(b.t, ok, "want *common.Page, got %T", p)

	return pp
}

// withHandler adds the given handler to the HTTP test server and makes it
// accessible with the given pattern.
func (b *testBrowser) withHandler(pattern string, handler http.HandlerFunc) *testBrowser {
	b.t.Helper()

	if b.http == nil {
		b.t.Fatalf("You should enable HTTP test server, see: withHTTPServer option")
	}
	b.http.Mux.Handle(pattern, handler)
	return b
}

const testBrowserStaticDir = "static"

// withFileServer serves a file server using the HTTP test server that is
// accessible via `testBrowserStaticDir` prefix.
//
// This method is for enabling the static file server after starting a test
// browser. For early starting the file server see withFileServer function.
func (b *testBrowser) withFileServer() *testBrowser {
	b.t.Helper()

	const (
		slash = string(os.PathSeparator)
		path  = slash + testBrowserStaticDir + slash
	)

	fs := http.FileServer(http.Dir(testBrowserStaticDir))

	return b.withHandler(path, http.StripPrefix(path, fs).ServeHTTP)
}

// URL returns the listening HTTP test server's URL combined with the given path.
func (b *testBrowser) URL(path string) string {
	b.t.Helper()

	if b.http == nil {
		b.t.Fatalf("You should enable HTTP test server, see: withHTTPServer option")
	}
	return b.http.ServerHTTP.URL + path
}

// staticURL is a helper for URL("/`testBrowserStaticDir`/"+ path).
func (b *testBrowser) staticURL(path string) string {
	b.t.Helper()

	return b.URL("/" + testBrowserStaticDir + "/" + path)
}

// Context returns the testBrowser context.
func (b *testBrowser) Context() context.Context {
	return b.ctx
}

// Cancel cancels the testBrowser context.
func (b *testBrowser) Cancel() {
	b.cancel()
}

// attachFrame attaches the frame to the page and returns it.
func (b *testBrowser) attachFrame(page *common.Page, frameID string, url string) *common.Frame {
	b.t.Helper()

	pageFn := `
	async (frameId, url) => {
		const frame = document.createElement('iframe');
		frame.src = url;
		frame.id = frameId;
		document.body.appendChild(frame);
		await new Promise(x => frame.onload = x);
		return frame;
	}
	`

	h, err := page.EvaluateHandle(
		b.toGojaValue(pageFn),
		b.toGojaValue(frameID),
		b.toGojaValue(url))
	require.NoError(b.t, err)

	f, err := h.AsElement().ContentFrame()
	require.NoError(b.t, err)

	ff, ok := f.(*common.Frame)
	require.Truef(b.t, ok, "want *common.Frame, got %T", f)

	return ff
}

// runtime returns a VU runtime.
func (b *testBrowser) runtime() *goja.Runtime { return b.vu.Runtime() }

// toGojaValue converts a value to goja value.
func (b *testBrowser) toGojaValue(i any) goja.Value { return b.runtime().ToValue(i) }

// asGojaValue asserts that v is a goja value and returns v as a goja.value.
func (b *testBrowser) asGojaValue(v any) goja.Value {
	b.t.Helper()
	gv, ok := v.(goja.Value)
	require.Truef(b.t, ok, "want goja.Value; got %T", v)
	return gv
}

// asGojaBool asserts that v is a boolean goja value and returns v as a boolean.
func (b *testBrowser) asGojaBool(v any) bool {
	b.t.Helper()
	gv := b.asGojaValue(v)
	require.IsType(b.t, b.toGojaValue(true), gv)
	return gv.ToBoolean()
}

// runJavaScript in the goja runtime.
func (b *testBrowser) runJavaScript(s string, args ...any) (goja.Value, error) {
	b.t.Helper()
	v, err := b.runtime().RunString(fmt.Sprintf(s, args...))
	if err != nil {
		return nil, fmt.Errorf("while running %q(%v): %w", s, args, err)
	}
	return v, nil
}

// Run the given functions in parallel and waits for them to finish.
func (b *testBrowser) run(ctx context.Context, fs ...func() error) error {
	b.t.Helper()

	g, ctx := errgroup.WithContext(ctx)
	for _, f := range fs {
		f := f
		g.Go(func() error {
			errc := make(chan error, 1)
			go func() { errc <- f() }()
			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("while running %T: %w", f, err)
				}
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("while waiting for %T: %w", fs, err)
	}

	return nil
}

// awaitWithTimeout is the same as await but takes a timeout and times out the function after the time runs out.
func (b *testBrowser) awaitWithTimeout(timeout time.Duration, fn func() error) error {
	b.t.Helper()
	errC := make(chan error)
	go func() {
		defer close(errC)
		errC <- fn()
	}()

	// use timer instead of time.After to not leak time.After for the duration of the timeout
	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case err := <-errC:
		return err
	case <-t.C:
		return fmt.Errorf("test timed out after %s", timeout)
	}
}

// httpServerOption is used to detect whether to enable the HTTP test
// server.
type httpServerOption struct{}

// withHTTPServer enables the HTTP test server.
//
// example:
//
//	b := TestBrowser(t, withHTTPServer())
func withHTTPServer() httpServerOption {
	return struct{}{}
}

// fileServerOption is used to detect whether enable the static file
// server.
type fileServerOption struct{}

// withFileServer enables the HTTP test server and serves a file server
// for static files.
//
// see: WithFileServer
//
// example:
//
//	b := TestBrowser(t, withFileServer())
func withFileServer() fileServerOption {
	return struct{}{}
}

// logCacheOption is used to detect whether to enable the log cache.
type logCacheOption struct{}

// withLogCache enables the log cache.
//
// example:
//
//	b := TestBrowser(t, withLogCache())
func withLogCache() logCacheOption {
	return struct{}{}
}

// skipCloseOption is used to indicate that we shouldn't call Browser.Close() in
// t.Cleanup(), since it will presumably be done by the test.
type skipCloseOption struct{}

// withSkipClose skips calling Browser.Close() in t.Cleanup().
//
// example:
//
//	b := TestBrowser(t, withSkipClose())
func withSkipClose() skipCloseOption {
	return struct{}{}
}

// withSamplesListener is used to indicate we want to use a bidirectional channel
// so that the test can read the metrics being emitted to the channel.
type withSamplesListener chan k6metrics.SampleContainer

func newBrowserTypeWithVU(tb testing.TB, opts *testBrowserOptions) (
	_ *chromium.BrowserType,
	_ *k6test.VU,
	cancel func(),
) {
	tb.Helper()

	// Prepare the VU.
	vu := k6test.NewVU(tb, k6test.WithSamplesListener(opts.samples))
	mi, ok := k6http.New().NewModuleInstance(vu).(*k6http.ModuleInstance)
	require.Truef(tb, ok, "want *k6http.ModuleInstance; got %T", mi)
	require.NoError(tb, vu.Runtime().Set("http", mi.Exports().Default))
	metricsCtx := k6ext.WithCustomMetrics(
		vu.Context(),
		k6ext.RegisterCustomMetrics(k6metrics.NewRegistry()),
	)
	ctx, cancel := context.WithCancel(metricsCtx)
	vu.CtxField = ctx
	vu.InitEnvField.LookupEnv = opts.lookupFunc

	bt := chromium.NewBrowserType(vu)
	// Delete the pre-init stage data.
	vu.MoveToVUContext()

	return bt, vu, cancel
}
