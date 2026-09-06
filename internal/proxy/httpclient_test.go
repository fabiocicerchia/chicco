package proxy

import (
	"io"
	"net/http"
	"testing"
)

// The tests here talk to httptest servers over real HTTP. http.Get and
// http.Post build a request with no context, so a server that never answers
// hangs until the whole package times out rather than until this test's
// deadline is reached -- which is also what noctx flags them for. These carry
// the test's context, and cancel with it.

// httpPost - POSTs a JSON body to url with the test's context. Every request
// in these tests is JSON; a content type parameter only ever carried the one
// value.
func httpPost(t *testing.T, url string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("building POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// httpGet - GETs url with the test's context.
func httpGet(t *testing.T, url string) (*http.Response, error) {
	return httpGetWith(t, http.DefaultClient, url)
}

// httpGetWith - httpGet through a specific client, for the tests that need one
// carrying a cookie jar.
func httpGetWith(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building GET %s: %v", url, err)
	}
	return c.Do(req)
}
