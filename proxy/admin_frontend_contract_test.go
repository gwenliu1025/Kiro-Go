package proxy

import (
	"os"
	"strings"
	"testing"
)

func readWebAsset(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestAdminFrontendCredentialImportPreservesProfileRouting(t *testing.T) {
	app := readWebAsset(t, "../web/app.js")
	for _, expected := range []string{
		"profileArn: c.profileArn || a.profileArn || ''",
		"profileRegionHint: c.profileRegionHint || a.profileRegionHint || ''",
		"profileArn: item.profileArn || ''",
		"profileRegionHint: item.profileRegionHint || ''",
	} {
		if !strings.Contains(app, expected) {
			t.Fatalf("app.js missing %q", expected)
		}
	}
	if strings.Contains(app, "if (!provider && authMethod === 'idc') provider = 'BuilderId';") {
		t.Fatal("IdC credential import must not default provider to BuilderId")
	}
}
