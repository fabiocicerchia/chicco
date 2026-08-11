package proxy

import "testing"

// chicco holds a working key for every provider, so "reachable" and
// "unauthenticated" must never be true at once. The default addr has to stay
// loopback for the same reason.
func TestRequireAuthOnBind(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		apiKey  string
		wantErr bool
	}{
		{"default addr, no key", DefaultAddr, "", false},
		{"loopback, no key", "127.0.0.1:41986", "", false},
		{"localhost, no key", "localhost:41986", "", false},
		{"ipv6 loopback, no key", "[::1]:41986", "", false},
		{"wildcard bare port, no key", ":41986", "", true},
		{"wildcard 0.0.0.0, no key", "0.0.0.0:41986", "", true},
		{"routable, no key", "192.168.1.10:41986", "", true},
		{"wildcard with key", ":41986", "s3cret", false},
		{"routable with key", "192.168.1.10:41986", "s3cret", false},
		{"unparseable, no key", "not-an-addr", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireAuthOnBind(tc.addr, tc.apiKey)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RequireAuthOnBind(%q, key=%t) = %v, wantErr %t",
					tc.addr, tc.apiKey != "", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultAddrIsLoopback(t *testing.T) {
	if !isLoopback(DefaultAddr) {
		t.Fatalf("DefaultAddr %q must be loopback: the default config has no api_key", DefaultAddr)
	}
}
