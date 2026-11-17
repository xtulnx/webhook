package app

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestRuntimeDirectoryDACL(t *testing.T) {
	const current = "S-1-5-21-100-200-300-1001"
	const other = "S-1-5-21-100-200-300-1002"
	user, err := windows.StringToSid(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sddl string
		ok   bool
	}{
		{"user-owned", "O:" + current + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + current + ")", true},
		{"admin-owned", "O:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + current + ")", true},
		{"other-account", "O:BAD:P(A;;GA;;;BA)(A;;GA;;;" + other + ")", false},
		{"current-user-on-other-directory", "O:" + other + "D:P(A;;GA;;;" + current + ")", false},
		{"everyone", "O:BAD:P(A;;GA;;;BA)(A;;GA;;;WD)", false},
		{"unprotected", "O:BAD:(A;;GA;;;BA)", false},
		{"empty", "O:BAD:P", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRuntimeDirectoryDACL(sd, user); (err == nil) != tc.ok {
				t.Fatalf("valid = %t, want %t, error = %v", err == nil, tc.ok, err)
			}
		})
	}
}
