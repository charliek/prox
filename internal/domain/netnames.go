package domain

import (
	"fmt"
	"net"
	"regexp"
)

// The naming rules a registration must satisfy, in the one place BOTH sides of
// a registration can reach (plan 031 F9).
//
// They live here for the same reason NormalizeHubURL does: internal/config
// applies them to a project's own `prox.yaml` before `prox up` ever calls the
// daemon, and internal/proxyd must apply the identical rules to a registration
// that arrived over the hub's NETWORK mount — where no local config file was
// ever consulted and the bytes came from another machine. Neither package may
// import the other, so a copy in each would be two rules that drift, and the
// half that drifts is the half a remote publisher talks to.
//
// Error texts are written to read correctly after a caller-supplied subject
// ("services.auth: ...", "service %q: ..."), so they do not repeat the offender
// themselves.

// hostnameRegex validates a hostname (an IP address is checked separately).
var hostnameRegex = regexp.MustCompile(`^(localhost|[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*)$`)

// domainNameRegex validates a proxy domain (basic DNS name validation).
var domainNameRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// ValidateServiceName checks a service name, which becomes a DNS label in the
// hostname `<service>.<domain>`: lowercase alphanumerics and hyphens, no
// leading or trailing hyphen, at most 63 characters.
//
// This is the rule that keeps a registration from minting a hostname nobody
// could have meant — "*.example.com", "..", a label with a slash in it — which
// matters most on the hub's network mount, where the name is composed into a
// route key and a certificate's SNI lookup.
func ValidateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("service name too long (max 63 characters)")
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("service name cannot start or end with hyphen")
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("service name can only contain lowercase letters, numbers, and hyphens")
		}
	}
	return nil
}

// ValidateHost checks a backend host: a literal IP address (v4 or v6) or a
// hostname.
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("host cannot be empty")
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	if !hostnameRegex.MatchString(host) {
		return fmt.Errorf("invalid host format %q", host)
	}
	return nil
}

// ValidatePort checks a backend port is in the usable range.
func ValidatePort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("must be between 1 and 65535, got %d", port)
	}
	return nil
}

// ValidateDomainName checks a proxy domain — the suffix every registered
// hostname is built on.
func ValidateDomainName(name string) error {
	if name == "" {
		return fmt.Errorf("domain cannot be empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("domain too long (max 253 characters)")
	}
	if !domainNameRegex.MatchString(name) {
		return fmt.Errorf("invalid domain format %q", name)
	}
	return nil
}
