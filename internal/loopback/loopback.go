// Package loopback checks that a URL stays on the local host.
package loopback

import (
	"net"
	"net/url"
	"strings"
)

// IsHTTPURL reports whether raw is an http or https URL whose host is
// localhost or a loopback IP address.
func IsHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
