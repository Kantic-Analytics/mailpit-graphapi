package loopback

import "testing"

func TestIsHTTPURL(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"http://127.0.0.1:8025", true},
		{"https://localhost/hook", true},
		{"http://LOCALHOST:8080", true},
		{"http://[::1]:8080/x", true},
		{"http://127.1.2.3", true},
		{"http://example.com", false},
		{"http://10.0.0.1:8025", false},
		{"http://localhost.example.com", false},
		{"ftp://127.0.0.1/", false},
		{"127.0.0.1:8025", false},
		{"http://%zz", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := IsHTTPURL(tt.raw); got != tt.want {
				t.Errorf("IsHTTPURL(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
