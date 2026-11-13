// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestUnescape(t *testing.T) {
	for path, unit := range map[string]string{
		"dev_2dvda_2edevice":                 "dev-vda.device",
		"system_2dserial_5cx2dgetty_2eslice": `system-serial\x2dgetty.slice`,
		"serial_2dgetty_40ttyS0_2eservice":   "serial-getty@ttyS0.service",
		"_2d_2emount":                        "-.mount",
	} {
		if got := unescape(path); got != unit {
			t.Errorf("unescape(%q) = %q, want %q", path, got, unit)
		}
	}
}

// An escape at the very end of a path is read, and an escape cut short is left as it is: a path
// is systemd's, and a unit name may end in one.
func TestUnescapeAtTheEnd(t *testing.T) {
	for path, unit := range map[string]string{
		"a_2e": "a.",
		"a_2":  "a_2",
		"a_":   "a_",
	} {
		if got := unescape(path); got != unit {
			t.Errorf("unescape(%q) = %q, want %q", path, got, unit)
		}
	}
}
