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

func TestAdminFrontendIamSsoContract(t *testing.T) {
	app := readWebAsset(t, "../web/app.js")
	for _, expected := range []string{
		`id="iamProfileRegion"`,
		`id="iamAuthRegion"`,
		`const profileRegion = $('iamProfileRegion').value.trim();`,
		`profileRegion,`,
		`authRegion: $('iamAuthRegion').value`,
		`t('subscription.unknown')`,
		`/auth/iam-sso/cancel`,
		`t('iam.profileRegionRequired')`,
	} {
		if !strings.Contains(app, expected) {
			t.Fatalf("app.js missing %q", expected)
		}
	}
	if strings.Contains(app, `id="iamCallbackRegion"`) {
		t.Fatal("callback region field must not be added")
	}
	if strings.Contains(app, `try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }`) {
		t.Fatal("automatic account refresh must not swallow errors")
	}

	styles := readWebAsset(t, "../web/styles.css")
	if !strings.Contains(styles, ".badge-unknown") {
		t.Fatal("styles.css missing unknown subscription badge")
	}
	if strings.Contains(styles, "var(--text-muted)") {
		t.Fatal("styles.css uses undefined --text-muted token")
	}

	for _, localePath := range []string{"../web/locales/zh.json", "../web/locales/en.json"} {
		locale := readWebAsset(t, localePath)
		for _, key := range []string{"iam.profileRegion", "iam.authRegion", "iam.profileRegionRequired", "subscription.unknown", "iam.refreshWarning"} {
			if !strings.Contains(locale, `"`+key+`"`) {
				t.Fatalf("%s missing locale key %s", localePath, key)
			}
		}
	}
}
