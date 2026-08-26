package catalog

import (
	"strings"
	"testing"
)

func TestMergePushedReleaseAssetsReplacesSameFileAndPreservesOtherPlatforms(t *testing.T) {
	current := []adminReleaseAssetRow{
		{Platform: "windows", Architecture: "x64", FileName: "desktop.zip", FileSizeBytes: 10, SHA256: strings.Repeat("a", 64), SignatureStatus: "unknown", ObjectKey: "releases/old/desktop.zip", DownloadURL: "https://old.example/desktop.zip"},
		{Platform: "linux", Architecture: "x64", FileName: "desktop.tar.gz", FileSizeBytes: 20, SHA256: strings.Repeat("b", 64), SignatureStatus: "unknown", ObjectKey: "local/desktop/1.0.0/linux/x64/" + strings.Repeat("b", 64) + "/desktop.tar.gz"},
	}
	pushed := []SaveReleaseAssetInput{{
		Platform: "windows", Architecture: "x64", FileName: "desktop.zip", FileSizeBytes: 11,
		SHA256: strings.Repeat("c", 64), SignatureStatus: "valid", Source: "local",
		ObjectKey: "local/desktop/1.0.0/windows/x64/" + strings.Repeat("c", 64) + "/desktop.zip",
	}}
	merged := mergePushedReleaseAssets(current, pushed)
	if len(merged) != 2 || merged[0].SHA256 != strings.Repeat("c", 64) || merged[0].Source != "local" ||
		merged[1].FileName != "desktop.tar.gz" || merged[1].Source != "local" {
		t.Fatalf("merged assets = %+v", merged)
	}
}

func TestPushedReleaseDefaultsUseProjectSoftwareName(t *testing.T) {
	for project, expected := range map[string]string{
		ReleaseProjectWeb:     "WenzWork 服务端",
		ReleaseProjectDesktop: "WenzWork 桌面端",
		ReleaseProjectMobile:  "WenzWork 手机端",
	} {
		if actual := defaultReleaseSoftwareName(project); actual != expected {
			t.Fatalf("defaultReleaseSoftwareName(%q) = %q, want %q", project, actual, expected)
		}
	}
}
