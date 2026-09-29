package fsimpl

import (
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/hairyhenderson/go-fsimpl/internal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file proves architectural invariants of the FSMux/provider/extension
// design using only in-memory filesystems (testing/fstest.MapFS) and tiny
// custom providers. It never touches the network, cloud services, the git
// daemon, or consul, and it does not change any public API.

// proofCapFS is an fs.FS that supports only WithContext, deliberately not
// fs.SubFS, so stdlib fs.Sub has to wrap it with its generic adapter.
type proofCapFS struct {
	base    fs.FS
	ctxMark string
}

var (
	_ fs.FS                  = (*proofCapFS)(nil)
	_ internal.WithContexter = (*proofCapFS)(nil)
)

func (f *proofCapFS) Open(name string) (fs.File, error) {
	return f.base.Open(name)
}

func (f *proofCapFS) WithContext(_ context.Context) fs.FS {
	c := *f
	c.ctxMark = "ctx-hit"

	return &c
}

func proofURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	require.NoError(t, err)

	return u
}

// Invariant 1: the mux map keyed by scheme means the last Add for a scheme
// wins, with no error or warning. Lookup dispatches purely on u.Scheme.
func TestUnderstandingProofDispatchOverride(t *testing.T) {
	first := fstest.MapFS{
		"who.txt": &fstest.MapFile{Data: []byte("first")},
	}
	second := fstest.MapFS{
		"who.txt": &fstest.MapFile{Data: []byte("second")},
	}

	mux := NewMux()
	mux.Add(FSProviderFunc(func(_ *url.URL) (fs.FS, error) { return &first, nil }, "proof"))
	mux.Add(FSProviderFunc(func(_ *url.URL) (fs.FS, error) { return &second, nil }, "proof"))

	got, err := mux.Lookup("proof:///bucket")
	require.NoError(t, err)

	dispatchedScheme := proofURL(t, "proof:///bucket").Scheme
	b, err := fs.ReadFile(got, "who.txt")
	require.NoError(t, err)

	t.Logf("dispatch scheme=%q winner=%q schemes=%v", dispatchedScheme, string(b), mux.Schemes())

	assert.Equal(t, []byte("second"), b, "later registration for the same scheme must override")
	assert.Same(t, &second, got)
	assert.Equal(t, []string{"proof"}, mux.Schemes(), "a map cannot hold two providers for one scheme")
}

// Invariant 2: WrappedFSProvider consumes u.Path at New time and roots the
// returned FS with fs.Sub. This proves (a) root URLs return the base FS
// unchanged, (b) non-root paths establish a real chroot boundary, (c) an
// encoded slash (%2F) is decoded into an ordinary separator by net/url, and
// (d) ".." segments are rejected by fs.Sub's ValidPath check.
func TestUnderstandingProofSubBoundary(t *testing.T) {
	base := fstest.MapFS{
		"root/sub/subsub/file.txt": &fstest.MapFile{Data: []byte("hello")},
		"rootfile.txt":             &fstest.MapFile{Data: []byte("rootfile")},
	}
	provider := WrappedFSProvider(&base, "mem")

	// (a) path "/" maps to ".", so fs.Sub returns the base FS itself.
	rootFS, err := provider.New(proofURL(t, "mem:///"))
	require.NoError(t, err)
	t.Logf("sub url=%q path=%q -> dir=\".\" sameAsBase=%t", "mem:///", "/", rootFS == fs.FS(&base))
	assert.Same(t, &base, rootFS)

	// (b) a non-root path roots the FS at that directory; siblings above are
	// unreachable because io/fs names cannot escape via "..".
	subFS, err := provider.New(proofURL(t, "mem:///root/sub"))
	require.NoError(t, err)
	t.Logf("sub url=%q path=%q -> rooted at root/sub", "mem:///root/sub", "/root/sub")

	b, err := fs.ReadFile(subFS, "subsub/file.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), b)

	_, err = subFS.Open("../rootfile.txt")
	t.Logf("boundary escape Open(%q) -> err=%v", "../rootfile.txt", err)
	assert.Error(t, err)

	entries, err := fs.ReadDir(subFS, ".")
	require.NoError(t, err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	t.Logf("readdir sub-root=%q entries=%v", "root/sub", names)
	assert.Equal(t, []string{"subsub"}, names)

	// (c) "%2F" lands in u.Path decoded, so WrappedFSProvider cannot tell it
	// apart from a real path separator.
	encoded := proofURL(t, "mem:///root%2Fsub")
	encFS, err := provider.New(encoded)
	require.NoError(t, err)
	b2, err := fs.ReadFile(encFS, "subsub/file.txt")
	require.NoError(t, err)
	t.Logf("encoded-slash url=%q u.Path=%q (decoded) -> same root as /root/sub, content=%q",
		"mem:///root%2Fsub", encoded.Path, string(b2))
	assert.Equal(t, []byte("hello"), b2)

	// (d) dot-segments surviving in u.Path are rejected by fs.Sub/ValidPath.
	_, err = provider.New(proofURL(t, "mem:///root/../etc"))
	t.Logf("dotdot url=%q u.Path=%q -> New err=%v", "mem:///root/../etc", "/root/../etc", err)
	assert.Error(t, err)
}

// Supporting observation for the optional-interface risk: capabilities are
// discovered only by runtime type assertion. A FS that implements
// WithContexter accepts the context; after a generic fs.Sub wrap (the base FS
// deliberately has no Sub method), that identity is lost, so all With*
// helpers silently return the object unchanged.
func TestUnderstandingProofCapabilityIdentity(t *testing.T) {
	backing := fstest.MapFS{
		"dir/file.txt": &fstest.MapFile{Data: []byte("deep")},
	}
	raw := &proofCapFS{base: backing}

	// Direct: WithContext hits, while header/client miss because the type
	// does not implement those interfaces.
	withCtx := WithContextFS(context.Background(), raw)
	capCtx, ok := withCtx.(*proofCapFS)
	require.True(t, ok)
	t.Logf("capability direct object=%T WithContext hit=%v WithHeader hit=%v WithHTTPClient hit=%v",
		raw, capCtx.ctxMark == "ctx-hit",
		sameAfter(WithHeaderFS(http.Header{}, raw), raw),
		sameAfter(WithHTTPClientFS(http.DefaultClient, raw), raw))
	assert.Equal(t, "ctx-hit", capCtx.ctxMark)
	assert.True(t, sameAfter(WithHeaderFS(http.Header{}, raw), raw))
	assert.True(t, sameAfter(WithHTTPClientFS(http.DefaultClient, raw), raw))

	// Subbed through WrappedFSProvider: the concrete type is replaced by the
	// stdlib fs.Sub adapter, which does not implement WithContexter.
	provider := WrappedFSProvider(raw, "cap")
	sub, err := provider.New(proofURL(t, "cap:///dir"))
	require.NoError(t, err)

	_, stillCapable := sub.(internal.WithContexter)
	after := WithContextFS(context.Background(), sub)
	b, err := fs.ReadFile(after, "file.txt")
	require.NoError(t, err)

	t.Logf("capability subbed object=%T stillImplementsWithContext=%v WithContext returnedSame=%v content=%q",
		sub, stillCapable, after == sub, string(b))
	assert.False(t, stillCapable, "fs.Sub adapter does not forward WithContexter")
	assert.Same(t, sub, after, "unsupported capability means the helper returns the original object")
	assert.Equal(t, []byte("deep"), b, "ordinary Open/Read still work through the adapter")
}

func sameAfter(got, want fs.FS) bool { return got == want }
