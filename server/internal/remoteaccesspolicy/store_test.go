package remoteaccesspolicy

import "testing"

func TestDeviceLimitValidation(t *testing.T) {
	for _, test := range []struct {
		value int
		valid bool
	}{
		{0, false}, {-1, false}, {1, true}, {DefaultDeviceLimit, true},
		{MaximumDeviceLimit, true}, {MaximumDeviceLimit + 1, false},
	} {
		if got := validDeviceLimit(test.value); got != test.valid {
			t.Fatalf("validDeviceLimit(%d) = %t; want %t", test.value, got, test.valid)
		}
	}
}
