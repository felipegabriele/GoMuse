//go:build windows || linux

package muse

import "tinygo.org/x/bluetooth"

// AddressHelp describes, for flag help and error messages, what an address is
// on this platform.
const AddressHelp = "a Bluetooth MAC address such as AA:BB:CC:DD:EE:FF"

// parseAddress turns the text of a peripheral address into the platform's
// address type. WinRT (Windows) and BlueZ (Linux) both identify a peripheral by
// its 48-bit Bluetooth address, unlike CoreBluetooth on macOS, which hides it
// behind a per-host UUID — so an address copied from `gomuse list` here is
// portable to another computer, which a macOS UUID is not.
//
// bluetooth.Address.Set swallows parse errors, so this parses explicitly: a
// typo should be an error, not a silent scan for the zero address.
func parseAddress(s string) (bluetooth.Address, error) {
	mac, err := bluetooth.ParseMAC(normalizeMAC(s))
	if err != nil {
		return bluetooth.Address{}, errBadAddress(s, err)
	}
	var addr bluetooth.Address
	addr.MAC = mac
	return addr, nil
}
