package environments

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// A public origin is the one thing a workload cannot work out for itself, so an
// operator declares it and this package decides what the declaration means. The
// decision is made once, here, by resolving the declaration the way the client
// that dials it resolves one — not by recognising spellings. A declared host is
// an address, or a name, or neither, and "neither" is a refusal.
//
// Every refusal names what was wrong with the declaration and never repeats the
// declared value: a declaration is operator input, it can carry a credential,
// and a render's diagnostics are written to a terminal and a log.

// OriginRefused is why a declaration is not a usable public origin. It carries a
// fixed phrase chosen from the reasons below and never any part of the input.
type OriginRefused struct {
	Reason string
}

func (refusal *OriginRefused) Error() string { return refusal.Reason }

// The reasons a declaration is refused. Each is a complete phrase so a caller
// can name the endpoint and append it.
const (
	originEmpty         = "declares no host"
	originNotWeb        = "does not declare an http or https origin"
	originHasCredential = "carries credentials in its URL"
	originNotBare       = "declares a path, query or fragment rather than a bare origin"
	originNoHost        = "names no host"
	originUnresolvable  = "names neither an address nor a public DNS name"
	originNotReachable  = "names an address nobody outside the workload reaches"
	originLocalName     = "names the machine the workload runs on"
	originBadPort       = "declares no usable port"
)

// ResolveOrigin turns one declared host into the canonical web origin a client
// would dial, or refuses it as not an origin at all.
//
// It answers one question — is this a well-formed web origin, and what is its
// canonical spelling — and deliberately not the other one. Whether an origin is
// a PUBLIC origin, reached by something other than the machine the workload runs
// on, is RequirePublicOrigin's question, because the answer differs by
// environment: a developer's own cluster is reached at a loopback origin and a
// cell is not, and a rule that conflated the two would refuse a local
// composition for declaring the truth about itself.
//
// The declaration may be a bare authority ("app.example.com", "app.example.com:8443")
// or a full origin ("https://app.example.com"). Everything else about it is
// decided rather than matched:
//
//   - the scheme must be http or https, so a non-web scheme is refused rather
//     than carried into a render;
//   - userinfo is refused before any diagnostic exists, because the diagnostic
//     would otherwise be where a credential is published;
//   - a path, query or fragment is refused: an origin is a scheme, a host and a
//     port, and a value carrying more is not the thing being declared;
//   - the host is normalised the way a browser normalises one — case folded,
//     one trailing dot dropped, and parsed as IPv4 whenever it *ends in a
//     number*, which is what makes "127.1", "2130706433", "0x7f000001" and
//     "0177.0.0.1" the loopback address rather than four exotic domain names;
//   - what is left must be a public DNS name, and a host that is neither a
//     parseable address nor a valid name is refused rather than passed through.
//
// The returned origin is canonical: lower-case host, bracketed IPv6 without its
// zone, and the scheme's default port dropped, so two declarations of the same
// origin compare equal.
func ResolveOrigin(declared string) (string, error) {
	trimmed := strings.TrimSpace(declared)
	if trimmed == "" {
		return "", &OriginRefused{Reason: originEmpty}
	}
	raw := trimmed
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", &OriginRefused{Reason: originUnresolvable}
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", &OriginRefused{Reason: originNotWeb}
	}
	// Before anything that could end up in a message.
	if parsed.User != nil || strings.Contains(trimmed, "@") {
		return "", &OriginRefused{Reason: originHasCredential}
	}
	if parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", &OriginRefused{Reason: originNotBare}
	}
	host, err := resolveHost(parsed.Hostname())
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(parsed.Scheme)
	port, err := canonicalPort(scheme, parsed.Port())
	if err != nil {
		return "", err
	}
	return scheme + "://" + host + port, nil
}

// canonicalPort keeps a port only when it is not the scheme's default, so
// "https://app.example.com:443" and "https://app.example.com" are one origin.
func canonicalPort(scheme, port string) (string, error) {
	if port == "" {
		return "", nil
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", &OriginRefused{Reason: originBadPort}
	}
	if (scheme == "https" && number == 443) || (scheme == "http" && number == 80) {
		return "", nil
	}
	return ":" + strconv.FormatUint(number, 10), nil
}

// resolveHost normalises and classifies a declared host: an address, a public
// name, or a refusal. It is the one place the question is decided.
func resolveHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", &OriginRefused{Reason: originNoHost}
	}
	// A trailing dot is the DNS root; one is dropped, two is not a name.
	host = strings.TrimSuffix(host, ".")
	if host == "" || strings.HasSuffix(host, ".") {
		return "", &OriginRefused{Reason: originUnresolvable}
	}
	if address, ok := parseHostAddress(host); ok {
		if address.Is6() {
			return "[" + address.String() + "]", nil
		}
		return address.String(), nil
	}
	// "localhost" and anything under it are reserved for the machine the client
	// runs on (RFC 6761). They are well-formed hosts — a local cluster is reached
	// at one — so they resolve here and are refused by RequirePublicOrigin, where
	// the question is whether an origin is one the outside world uses.
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return host, nil
	}
	if !publicDNSName(host) {
		return "", &OriginRefused{Reason: originUnresolvable}
	}
	return host, nil
}

// parseHostAddress parses a host as an address the way a client dialing it
// would: an IPv6 literal, or — whenever the host ends in a number — the IPv4
// forms every resolver accepts, with each part decimal, octal ("0…") or
// hexadecimal ("0x…"). A zone is dropped: it names an interface of one machine,
// which an origin cannot carry.
func parseHostAddress(host string) (netip.Addr, bool) {
	if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return address.Unmap().WithZone(""), true
	}
	address, ok := parseIPv4(host)
	if !ok {
		return netip.Addr{}, false
	}
	return address, true
}

// parseIPv4 implements the host parser's IPv4 rule: split on dots, drop one
// trailing empty part, and if the LAST part is a number the host is an address —
// every part must then be a number, and the last one absorbs the parts the host
// left out. A host whose last part is not a number is a name, not a malformed
// address, which is what keeps "app.example.com" out of this path.
func parseIPv4(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	if _, ok := parseIPv4Number(parts[len(parts)-1]); !ok {
		return netip.Addr{}, false
	}
	numbers := make([]uint64, 0, len(parts))
	for _, part := range parts {
		number, ok := parseIPv4Number(part)
		if !ok {
			return netip.Addr{}, false
		}
		numbers = append(numbers, number)
	}
	var value uint64
	for index, number := range numbers {
		if index == len(numbers)-1 {
			// The last part carries every octet the host did not spell out.
			limit := uint64(1) << (8 * (5 - len(numbers)))
			if number >= limit {
				return netip.Addr{}, false
			}
			value += number
			break
		}
		if number > 255 {
			return netip.Addr{}, false
		}
		value += number << (8 * (3 - index))
	}
	return netip.AddrFrom4([4]byte{
		byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value),
	}), true
}

// parseIPv4Number reads one part of an IPv4 host: hexadecimal with an "0x"
// prefix, octal with a leading zero, decimal otherwise.
func parseIPv4Number(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	base := 10
	digits := part
	switch {
	case strings.HasPrefix(part, "0x"):
		base, digits = 16, part[2:]
	case part[0] == '0' && len(part) > 1:
		base, digits = 8, part[1:]
	}
	if digits == "" {
		// "0x" alone, or "0": a zero part either way.
		return 0, true
	}
	number, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

// reachableAddress reports whether an address is one something outside the
// workload can reach. Loopback and the unspecified address name the machine the
// workload runs on; a link-local address names its link; a multicast address
// names a group rather than a host. None of them is an origin anybody else can
// use, so none of them is a public origin.
func reachableAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	return !address.IsLoopback() &&
		!address.IsUnspecified() &&
		!address.IsLinkLocalUnicast() &&
		!address.IsLinkLocalMulticast() &&
		!address.IsInterfaceLocalMulticast() &&
		!address.IsMulticast()
}

// publicDNSName reports whether a normalised host is a DNS name something
// outside the cell could resolve: ASCII letters, digits and hyphens in labels of
// 1..63 characters, no label starting or ending with a hyphen, at most 253
// characters, and more than one label — a single label resolves only inside
// whatever namespace the client happens to sit in, so it names no public origin.
//
// A host whose last label is all digits is never a name. The host parser reads
// such a host as an address (parseIPv4), so one that reaches here is an address
// that failed to parse — "1.2.3.4.5", "256.0.0.1", "08.0.0.1" — and calling it a
// domain would admit exactly the inputs that parse gave up on.
func publicDNSName(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	if allDigits(labels[len(labels)-1]) {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := 0; index < len(label); index++ {
			char := label[index]
			switch {
			case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-':
			default:
				return false
			}
		}
	}
	return true
}

// allDigits reports whether every character of a label is an ASCII digit.
func allDigits(label string) bool {
	if label == "" {
		return false
	}
	for index := 0; index < len(label); index++ {
		if label[index] < '0' || label[index] > '9' {
			return false
		}
	}
	return true
}

// RequirePublicOrigin refuses a well-formed origin that is not one something
// outside the workload reaches: the machine it runs on, its link, or a multicast
// group. It is asked only where the answer matters — a deployed cell — and takes
// an origin ResolveOrigin has already canonicalised.
func RequirePublicOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return &OriginRefused{Reason: originUnresolvable}
	}
	host := strings.ToLower(strings.Trim(parsed.Hostname(), "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return &OriginRefused{Reason: originLocalName}
	}
	if address, ok := parseHostAddress(host); ok && !reachableAddress(address) {
		return &OriginRefused{Reason: originNotReachable}
	}
	return nil
}

// OriginRefusalReason returns the fixed phrase of an origin refusal. An error
// that is not one answers with a phrase of its own rather than with the empty
// string: a caller builds a sentence out of this, and the value is what must
// never appear in it.
func OriginRefusalReason(err error) string {
	if err == nil {
		return ""
	}
	var refusal *OriginRefused
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return originUnresolvable
}

// describeOriginRefusal is the shape every caller's message takes: what was
// declared for, and what is wrong with it.
func describeOriginRefusal(subject string, err error) error {
	if reason := OriginRefusalReason(err); reason != "" {
		return fmt.Errorf("%s %s", subject, reason)
	}
	return fmt.Errorf("%s cannot be resolved: %w", subject, err)
}
