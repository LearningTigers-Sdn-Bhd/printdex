package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsNewer(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"v1.10.0", "1.9.3", true}, // numeric, not alphabetical
		{"1.2", "1.2.0", false},
		{"1.1.9", "1.2.0", false},
		{"2.0.0", "dev", true},
	} {
		if got := IsNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v", tc.latest, tc.current, got)
		}
	}
}

func TestSignedBuildVerifiesAndTamperedOneDoesNot(t *testing.T) {
	public, private, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "printdex")
	os.WriteFile(file, []byte("new build"), 0o600)
	signature, err := Sign(private, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(public, []byte("new build"), signature); err != nil {
		t.Errorf("genuine build rejected: %v", err)
	}
	if Verify(public, []byte("tampered build"), signature) == nil {
		t.Error("tampered build accepted")
	}
	if Verify("", []byte("new build"), signature) == nil {
		t.Error("build accepted without a public key")
	}
}

func TestUnkeyedAndDevBuildsDoNotSelfUpdate(t *testing.T) {
	if New("1.0.0", "").Status().UpdateSupported || New("dev", "key").Status().UpdateSupported {
		t.Error("self-update offered without a key or on a dev build")
	}
	if status := New("1.0.0", "").Download(); status.Phase != "error" {
		t.Errorf("download started on an unkeyed build: %+v", status)
	}
}
