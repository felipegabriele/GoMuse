package muse

import (
	"fmt"
	"strings"
)

// normalizeMAC turns the spellings people actually type or paste — Windows
// style "aa-bb-cc-dd-ee-ff", bare "AABBCCDDEEFF", lower case — into the one
// form bluetooth.ParseMAC accepts, "AA:BB:CC:DD:EE:FF". It only reshapes the
// text; whether the result is a valid MAC is still ParseMAC's call.
//
// It lives outside the per-OS address files so the reshaping is tested on every
// platform, not just the ones where a MAC address is the native identifier.
func normalizeMAC(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch {
	case len(s) == 12 && !strings.ContainsAny(s, ":-"):
		parts := make([]string, 6)
		for i := range parts {
			parts[i] = s[i*2 : i*2+2]
		}
		return strings.Join(parts, ":")
	default:
		return strings.ReplaceAll(s, "-", ":")
	}
}

// errBadAddress wraps a parse failure with the user-supplied text and what this
// platform expects, so a typo is reported rather than silently scanning for a
// zero address.
func errBadAddress(s string, err error) error {
	return fmt.Errorf("parse address %q (expected %s): %w", s, AddressHelp, err)
}
