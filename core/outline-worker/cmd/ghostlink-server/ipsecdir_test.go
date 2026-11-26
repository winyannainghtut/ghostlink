// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package main

import (
	"path/filepath"
	"testing"
)

// --ipsec-dir is empty (the default, <state>/ipsec) or an absolute path as the host script wrote
// it; anything else is refused before the server starts.
func TestCheckIPsecDir(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "ghostlink-ipsec")
	for _, good := range []string{"", absolute} {
		if err := checkIPsecDir(good); err != nil {
			t.Errorf("%q should be accepted: %v", good, err)
		}
	}
	for _, bad := range []string{"ipsec", "./ipsec", "ghostlink-ipsec/", absolute + string(filepath.Separator), absolute + "/../x", filepath.Join(absolute, "..") + "/x/./y"} {
		if err := checkIPsecDir(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
