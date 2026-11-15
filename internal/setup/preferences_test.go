package setup

import (
	"path/filepath"
	"testing"
)

func TestPreferencesRejectSecretsAndUnsafePaths(t *testing.T) {
	for _, p := range []Preferences{
		{Flags: map[string]string{"join-code": "secret"}},
		{Homes: map[string]string{"OPENAI_API_KEY": "secret"}},
		{Flags: map[string]string{"codex-root": "relative"}},
		{Flags: map[string]string{"no-service": "yes"}},
		{Homes: map[string]string{"CODEX_HOME": "relative"}},
	} {
		if err := SavePreferences(t.TempDir(), p); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	state := filepath.Join(t.TempDir(), "absent")
	if _, err := LoadPreferences(state); err != nil {
		t.Fatal(err)
	}
}
