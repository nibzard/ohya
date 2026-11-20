package buildinfo

import "testing"

func TestReleaseVersionWins(t *testing.T) {
	saved := ReleaseVersion
	t.Cleanup(func() { ReleaseVersion = saved })
	ReleaseVersion = "v9.9.9-test"
	version, _, _, _ := Identity()
	if version != ReleaseVersion {
		t.Fatalf("Identity version = %q; want the linked release version %q", version, ReleaseVersion)
	}
}

func TestIdentityAlwaysReportsAVersion(t *testing.T) {
	version, _, _, _ := Identity()
	if version == "" {
		t.Fatal("Identity version is empty")
	}
}
