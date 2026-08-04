package random

import (
	"regexp"
	"testing"
)

// The rule Docker Compose applies to a project name. AppManagement calls
// Name() to name a compose project when the user did not supply one, so
// anything Name() can return has to satisfy this.
var composeProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Exhaustive rather than sampled. Four of the contributor names carry
// diacritics, so 596 of the 40677 possible combinations used to be rejected by
// Docker — roughly one draw in seventy. A sampling test would have passed most
// of the time, which is exactly why this failed only occasionally in CI and
// nowhere reproducibly.
func TestEveryPossibleNameIsAValidComposeProjectName(t *testing.T) {
	for _, adjective := range left {
		for _, name := range right {
			suffix := "1"
			got := nameFrom(adjective, name, &suffix)
			if !composeProjectName.MatchString(got) {
				t.Errorf("invalid compose project name %q (from %q + %q)", got, adjective, name)
			}
			if got := nameFrom(adjective, name, nil); !composeProjectName.MatchString(got) {
				t.Errorf("invalid compose project name %q (from %q + %q)", got, adjective, name)
			}
		}
	}
}

func TestNameReturnsSomethingUsable(t *testing.T) {
	for i := 0; i < 200; i++ {
		if got := Name(nil); !composeProjectName.MatchString(got) {
			t.Fatalf("Name() returned %q, which Docker Compose would reject", got)
		}
	}
	suffix := "abc"
	if got := Name(&suffix); !composeProjectName.MatchString(got) {
		t.Fatalf("Name(suffix) returned %q, which Docker Compose would reject", got)
	}
}
