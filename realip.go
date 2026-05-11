package main

import (
	"net"
	"net/http"
	"strings"
)

// parsedTrustedProxies holds the parsed CIDR networks from the --trusted-proxies flag.
var parsedTrustedProxies []*net.IPNet

// initTrustedProxies parses the --trusted-proxies flag value into CIDR networks.
// Must be called after flag.Parse().
func initTrustedProxies() {
	if *trustedProxies == "" {
		return
	}

	for _, entry := range strings.Split(*trustedProxies, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// If it's a plain IP without CIDR notation, add /32 (IPv4) or /128 (IPv6).
		if !strings.Contains(entry, "/") {
			if strings.Contains(entry, ":") {
				entry += "/128"
			} else {
				entry += "/32"
			}
		}

		_, cidr, err := net.ParseCIDR(entry)
		if err != nil {
			continue
		}
		parsedTrustedProxies = append(parsedTrustedProxies, cidr)
	}
}

// isTrustedProxy checks whether the given remote address (IP:port) belongs to
// a trusted proxy network.
func isTrustedProxy(remoteAddr string) bool {
	if len(parsedTrustedProxies) == 0 {
		return false
	}

	ip := extractIP(remoteAddr)
	if ip == nil {
		return false
	}

	for _, cidr := range parsedTrustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveRealIP determines the real client IP address. If the request comes
// from a trusted proxy and --real-ip-header is configured, the IP is read from
// that header. Otherwise the TCP remote address is used.
func resolveRealIP(r *http.Request) string {
	if *realIPHeader != "" && isTrustedProxy(r.RemoteAddr) {
		headerVal := strings.TrimSpace(r.Header.Get(*realIPHeader))
		if headerVal != "" {
			// X-Forwarded-For may contain multiple IPs; take the first one.
			if idx := strings.IndexByte(headerVal, ','); idx != -1 {
				headerVal = strings.TrimSpace(headerVal[:idx])
			}
			return headerVal
		}
	}
	return r.RemoteAddr
}

// extractIP parses an IP from an address string that may include a port.
func extractIP(addr string) net.IP {
	addr = strings.Trim(addr, " []")

	// Try host:port split first.
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return net.ParseIP(host)
	}

	return net.ParseIP(addr)
}
