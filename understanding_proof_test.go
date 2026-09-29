package fsimpl

import (
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingProvider records the URL it receives from FSMux so the test can
// show exactly which URL parts survive dispatch.
type recordingProvider struct {
	lastURL *url.URL
	fsys    fs.FS
}

func (p *recordingProvider) Schemes() []string { return []string{"probe"} }

func (p *recordingProvider) New(u *url.URL) (fs.FS, error) {
	p.lastURL = u

	return p.fsys, nil
}

// ctxAwareFS is an in-memory fs.FS that supports the WithContext extension.
// Like the real providers (httpfs, blobfs, ...), WithContext returns a
// reconfigured copy rather than mutating the receiver.
type ctxAwareFS struct {
	fstest.MapFS
	ctxHits int
}

func (f *ctxAwareFS) WithContext(_ context.Context) fs.FS {
	f.ctxHits++

	return &ctxAwareFS{MapFS: f.MapFS}
}

// ctxOnlyFS supports WithContext but deliberately does NOT implement fs.SubFS,
// so fs.Sub must fall back to the generic wrapper which hides the extension.
type ctxOnlyFS struct {
	fsys    fs.FS
	ctxHits int
}

func (f *ctxOnlyFS) Open(name string) (fs.File, error) { return f.fsys.Open(name) }

func (f *ctxOnlyFS) WithContext(_ context.Context) fs.FS {
	f.ctxHits++

	return &ctxOnlyFS{fsys: f.fsys}
}

// Invariant 1: FSMux consumes only the URL scheme; host/path/query/fragment
// are passed to the provider untouched, and WrappedFSProvider maps a non-root
// URL path onto fs.Sub while the root path "/" preserves object identity.
func TestUnderstandingProofMuxDispatchAndSubPath(t *testing.T) {
	base := &fstest.MapFS{
		"root/sub/file.txt": &fstest.MapFile{Data: []byte("hello")},
		"rootfile.txt":      &fstest.MapFile{Data: []byte("root")},
	}

	mux := NewMux()
	mux.Add(WrappedFSProvider(base, "proof"))

	probe := &recordingProvider{fsys: base}
	mux.Add(probe)

	// the mux consumes only the scheme; everything else reaches the provider
	_, err := mux.Lookup("probe://example.com:8080/root/sub?x=1#frag")
	require.NoError(t, err)
	require.NotNil(t, probe.lastURL)
	t.Logf("dispatch: scheme=%q consumed by FSMux; provider received host=%q path=%q query=%q fragment=%q",
		probe.lastURL.Scheme, probe.lastURL.Host, probe.lastURL.Path,
		probe.lastURL.RawQuery, probe.lastURL.Fragment)
	assert.Equal(t, "probe", probe.lastURL.Scheme)
	assert.Equal(t, "example.com:8080", probe.lastURL.Host)
	assert.Equal(t, "/root/sub", probe.lastURL.Path)
	assert.Equal(t, "frag", probe.lastURL.Fragment)

	// root path "/" -> fs.Sub(fsys, ".") -> identical object
	root, err := mux.Lookup("proof:///")
	require.NoError(t, err)
	assert.Same(t, base, root)
	t.Logf("sub path: url path %q -> dir %q -> same object (%p)", "/", ".", base)

	// non-root path -> fs.Sub into the subtree
	sub, err := mux.Lookup("proof:///root/sub")
	require.NoError(t, err)
	assert.NotSame(t, base, sub)
	b, err := fs.ReadFile(sub, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))
	t.Logf("sub path: url path %q -> dir %q -> ReadFile(%q)=%q", "/root/sub", "root/sub", "file.txt", b)

	// an encoded slash is decoded by url.Parse BEFORE WrappedFSProvider sees
	// the path, so a single URL segment can cross a directory boundary
	enc, err := mux.Lookup("proof:///root%2Fsub")
	require.NoError(t, err)
	b, err = fs.ReadFile(enc, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))
	u, _ := url.Parse("proof:///root%2Fsub")
	t.Logf("sub path: escaped=%q decoded=%q -> fs.Sub dir %q (segment boundary crossed)",
		u.EscapedPath(), u.Path, "root/sub")

	// ".." segments are rejected by fs.Sub's ValidPath check
	_, err = mux.Lookup("proof:///root/../rootfile.txt")
	require.Error(t, err)
	t.Logf("sub path: url path %q -> fs.Sub error: %v", "/root/../rootfile.txt", err)
}

// Invariant 2: the With* extensions are best-effort. On a capability miss
// they return the identical object; on a hit they return a new object. A
// generic fs.Sub wrapper hides the optional interface, so the extension
// silently becomes a no-op.
func TestUnderstandingProofCapabilityIdentity(t *testing.T) {
	ctx := context.Background()

	plain := &fstest.MapFS{"a.txt": &fstest.MapFile{Data: []byte("a")}}

	// capability miss -> identical object back
	got := WithContextFS(ctx, plain)
	assert.Same(t, plain, got)
	assert.Same(t, plain, WithHeaderFS(http.Header{"X": {"1"}}, plain))
	assert.Same(t, plain, WithHTTPClientFS(&http.Client{}, plain))
	t.Logf("capability: %T has no WithContext/WithHeader/WithHTTPClient -> miss, identity preserved (%p)",
		plain, plain)

	// capability hit -> new object, hit recorded
	aware := &ctxAwareFS{MapFS: *plain}
	wrapped := WithContextFS(ctx, aware)
	assert.NotSame(t, aware, wrapped)
	assert.Equal(t, 1, aware.ctxHits)
	t.Logf("capability: %T implements WithContext -> hit=%d, new object returned (%p -> %p)",
		aware, aware.ctxHits, aware, wrapped)

	// generic fs.Sub wrapper hides the optional interface
	ctxOnly := &ctxOnlyFS{fsys: plain}
	sub, err := fs.Sub(ctxOnly, "sub")
	require.NoError(t, err)
	_, ok := sub.(interface{ WithContext(context.Context) fs.FS })
	assert.False(t, ok, "generic fs.Sub wrapper must not expose WithContext")

	out := WithContextFS(ctx, sub)
	assert.Same(t, sub, out)
	assert.Equal(t, 0, ctxOnly.ctxHits)
	t.Logf("capability: fs.Sub(%T) -> %T hides WithContext -> miss, hit=%d, identity preserved",
		ctxOnly, sub, ctxOnly.ctxHits)
}
