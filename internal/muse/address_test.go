package muse

import "testing"

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"AA:BB:CC:DD:EE:FF": "AA:BB:CC:DD:EE:FF",
		"aa:bb:cc:dd:ee:ff": "AA:BB:CC:DD:EE:FF",
		"aa-bb-cc-dd-ee-ff": "AA:BB:CC:DD:EE:FF",
		"AABBCCDDEEFF":      "AA:BB:CC:DD:EE:FF",
		"  aabbccddeeff \n": "AA:BB:CC:DD:EE:FF",
		// Anything else is passed through upper-cased for ParseMAC to reject.
		"AA:BB:CC":                  "AA:BB:CC",
		"not-an-address":            "NOT:AN:ADDRESS",
		"":                          "",
		"AABBCCDDEEFFAA":            "AABBCCDDEEFFAA",
		"AA:BB:CC:DD:EE:FF:00":      "AA:BB:CC:DD:EE:FF:00",
		"AABBCCDDEEF":               "AABBCCDDEEF",
		"AA-BB-CC-DD-EE-FF-00-11-2": "AA:BB:CC:DD:EE:FF:00:11:2",
	}
	for in, want := range cases {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
