package catalog

import "testing"

func TestValidReleaseDeliverySettings(t *testing.T) {
	tests := []struct {
		mode, prefix string
		valid        bool
	}{
		{ReleaseDownloadProxyCached, "", true},
		{ReleaseDownloadProxyCached, "https://cdn.example.test/files", true},
		{ReleaseDownloadS3Redirect, "https://cdn.example.test/files", true},
		{ReleaseDownloadS3Redirect, "", false},
		{ReleaseDownloadS3Redirect, "javascript:alert(1)", false},
		{ReleaseDownloadS3Redirect, "https://user:password@example.test/files", false},
		{ReleaseDownloadGitHubRedirect, "", true},
		{ReleaseDownloadGitHubRedirect, "https://cdn.example.test/files", true},
		{"unknown", "https://cdn.example.test", false},
	}
	for _, test := range tests {
		if got := validReleaseDeliverySettings(test.mode, test.prefix); got != test.valid {
			t.Errorf("validReleaseDeliverySettings(%q, %q) = %v, want %v", test.mode, test.prefix, got, test.valid)
		}
	}
}
