//go:build windows || linux

package muse

import "testing"

func TestParseAddressMAC(t *testing.T) {
	for _, in := range []string{
		"00:55:DA:B0:12:34", "00:55:da:b0:12:34", "00-55-DA-B0-12-34", "0055DAB01234",
	} {
		addr, err := parseAddress(in)
		if err != nil {
			t.Fatalf("parseAddress(%q): %v", in, err)
		}
		if got, want := addr.String(), "00:55:DA:B0:12:34"; got != want {
			t.Errorf("parseAddress(%q).String() = %q, want %q", in, got, want)
		}
	}
}

func TestParseAddressRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"", "MuseS-1234", "01234567-89AB-CDEF-0123-456789ABCDEF", "00:55:DA:B0:12", "ZZ:55:DA:B0:12:34",
	} {
		if _, err := parseAddress(in); err == nil {
			t.Errorf("parseAddress(%q) succeeded, want an error", in)
		}
	}
}

// A scanned address is re-parsed from its own String form; the two must agree.
func TestScannedAddressRoundTrip(t *testing.T) {
	addr, err := scannedAddress(Found{Name: "MuseS-1234", Address: "00:55:DA:B0:12:34"})
	if err != nil {
		t.Fatal(err)
	}
	if got := addr.String(); got != "00:55:DA:B0:12:34" {
		t.Errorf("round trip = %q", got)
	}
	if _, err := scannedAddress(Found{Name: "x", Address: "bogus"}); err == nil {
		t.Error("scannedAddress accepted a bogus address")
	}
}
