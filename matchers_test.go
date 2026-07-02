package caddy_docker_upstreams

import (
	"context"
	"net/http"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestContext(t *testing.T) caddy.Context {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	return ctx
}

// prepareRequest attaches the replacer and vars that caddy's matchers expect to
// find in the request context (mirroring caddyhttp.PrepareRequest).
func prepareRequest(r *http.Request) *http.Request {
	repl := caddy.NewReplacer()
	ctx := context.WithValue(r.Context(), caddy.ReplacerCtxKey, repl)
	ctx = context.WithValue(ctx, caddyhttp.VarsCtxKey, map[string]any{})
	return r.WithContext(ctx)
}

func newRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	require.NoError(t, err, "building request")
	return prepareRequest(req)
}

func TestProducers(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		value   string
		req     *http.Request
		want    bool
		wantErr bool
	}{
		{
			name:  "host matches",
			label: LabelMatchHost,
			value: "example.com",
			req:   newRequest(t, http.MethodGet, "http://example.com/"),
			want:  true,
		},
		{
			name:  "host does not match",
			label: LabelMatchHost,
			value: "example.com",
			req:   newRequest(t, http.MethodGet, "http://other.com/"),
			want:  false,
		},
		{
			name:  "host matches one of many",
			label: LabelMatchHost,
			value: "a.example.com b.example.com",
			req:   newRequest(t, http.MethodGet, "http://b.example.com/"),
			want:  true,
		},
		{
			name:  "method matches",
			label: LabelMatchMethod,
			value: "GET POST",
			req:   newRequest(t, http.MethodPost, "http://example.com/"),
			want:  true,
		},
		{
			name:  "method does not match",
			label: LabelMatchMethod,
			value: "GET",
			req:   newRequest(t, http.MethodDelete, "http://example.com/"),
			want:  false,
		},
		{
			name:  "path matches",
			label: LabelMatchPath,
			value: "/api/*",
			req:   newRequest(t, http.MethodGet, "http://example.com/api/users"),
			want:  true,
		},
		{
			name:  "path does not match",
			label: LabelMatchPath,
			value: "/api/*",
			req:   newRequest(t, http.MethodGet, "http://example.com/web"),
			want:  false,
		},
		{
			name:  "query matches",
			label: LabelMatchQuery,
			value: "debug=true",
			req:   newRequest(t, http.MethodGet, "http://example.com/?debug=true"),
			want:  true,
		},
		{
			name:  "query does not match",
			label: LabelMatchQuery,
			value: "debug=true",
			req:   newRequest(t, http.MethodGet, "http://example.com/?debug=false"),
			want:  false,
		},
		{
			name:    "query invalid value",
			label:   LabelMatchQuery,
			value:   "%zz",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			producer, ok := producers[tt.label]
			require.Truef(t, ok, "no producer registered for label %q", tt.label)

			matcher, err := producer(tt.value)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			got, err := matcher.MatchWithError(tt.req)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildMatchers(t *testing.T) {
	ctx := newTestContext(t)

	labels := map[string]string{
		LabelMatchHost:   "example.com",
		LabelMatchMethod: "GET",
		LabelMatchPath:   "/api/*",
		// A label that is not a matcher must be ignored.
		LabelEnable: "true",
	}

	matchers := buildMatchers(ctx, labels)
	require.Len(t, matchers, 3)

	ok, err := matchers.MatchWithError(newRequest(t, http.MethodGet, "http://example.com/api/users"))
	require.NoError(t, err)
	assert.True(t, ok, "expected request to match all matchers")

	// Wrong method: the matcher set must not match.
	ok, err = matchers.MatchWithError(newRequest(t, http.MethodPost, "http://example.com/api/users"))
	require.NoError(t, err)
	assert.False(t, ok, "expected request with wrong method not to match")
}

func TestBuildMatchersEmpty(t *testing.T) {
	ctx := newTestContext(t)

	matchers := buildMatchers(ctx, map[string]string{"unrelated": "label"})
	require.Empty(t, matchers)

	// An empty matcher set matches every request.
	ok, err := matchers.MatchWithError(newRequest(t, http.MethodGet, "http://anything/"))
	require.NoError(t, err)
	assert.True(t, ok, "expected empty matcher set to match any request")
}

var _ caddyhttp.RequestMatcherWithError = (caddyhttp.MatcherSet)(nil)
