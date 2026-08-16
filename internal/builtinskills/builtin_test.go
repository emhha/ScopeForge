package builtinskills

import (
	"strings"
	"testing"
)

func TestKaliToolsSkillPresent(t *testing.T) {
	items, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Name == "kali-tools" {
			if !strings.Contains(it.Body, "sqlmap") || !strings.Contains(it.Body, "scopeforge-attacker") {
				t.Fatalf("kali-tools skill content incomplete:\n%s", it.Body)
			}
			return
		}
	}
	t.Fatal("kali-tools built-in skill missing")
}
