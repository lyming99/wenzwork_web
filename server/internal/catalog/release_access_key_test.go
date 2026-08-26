package catalog

import (
	"strings"
	"testing"
)

func TestReleaseAccessKeyDigestAcceptsOnlyGeneratedShape(t *testing.T) {
	valid := "release_" + strings.Repeat("a", 43)
	digest, ok := releaseAccessKeyDigest(valid)
	if !ok || len(digest) != 64 || digest == valid {
		t.Fatalf("valid key digest = %q, ok = %v", digest, ok)
	}
	if repeated, repeatedOK := releaseAccessKeyDigest(valid); !repeatedOK || repeated != digest {
		t.Fatalf("release access key digest was not stable: %q / %q", digest, repeated)
	}

	for _, invalid := range []string{
		"",
		"release_short",
		"relay_" + strings.Repeat("a", 43),
		"release_" + strings.Repeat("!", 43),
		"release_" + strings.Repeat("a", 44),
	} {
		if invalidDigest, invalidOK := releaseAccessKeyDigest(invalid); invalidOK || invalidDigest != "" {
			t.Errorf("invalid key %q produced digest %q", invalid, invalidDigest)
		}
	}
}

func TestReleaseAccessKeySettingsProjectionContainsNoDigest(t *testing.T) {
	settings := releaseAccessKeySettingsFromRow(releaseAccessKeySettingsRow{
		Initialized: true, AccessKeyDigest: strings.Repeat("b", 64), KeyPrefix: "release_abcdefgh",
		Version: 4,
	})
	if !settings.AccessKeyConfigured || settings.KeyPrefix != "release_abcdefgh" || settings.Version != 4 {
		t.Fatalf("settings projection = %+v", settings)
	}
}
