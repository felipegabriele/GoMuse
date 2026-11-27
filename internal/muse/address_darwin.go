//go:build darwin

package muse

import "tinygo.org/x/bluetooth"

// AddressHelp describes, for flag help and error messages, what an address is
// on this platform.
const AddressHelp = "a CoreBluetooth peripheral UUID"

// parseAddress turns the text of a peripheral address into the platform's
// address type. CoreBluetooth never exposes a Bluetooth MAC address; it assigns
// each peripheral a per-host UUID instead.
//
// bluetooth.Address.Set swallows parse errors, so this parses explicitly: a
// typo should be an error, not a silent scan for the zero UUID.
func parseAddress(s string) (bluetooth.Address, error) {
	uuid, err := bluetooth.ParseUUID(s)
	if err != nil {
		return bluetooth.Address{}, errBadAddress(s, err)
	}
	return bluetooth.Address{UUID: uuid}, nil
}
