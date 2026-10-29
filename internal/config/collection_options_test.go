package config

import (
	"testing"
)

// The catalog must not enable destructive collection unless its environment
// switch explicitly opts in. This tests the config boundary that reads the switch.
func TestSupersededProjectionCollectionSwitchRequiresExplicitEnable(t *testing.T) {
	for _, test := range []struct {
		name, value string
		want        bool
	}{
		{name: "unset", value: "", want: false},
		{name: "enabled", value: "on", want: true},
		{name: "other", value: "off", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NANO_SUPERSEDED_COLLECTION", test.value)
			if got := SupersededProjectionCollectionEnabled(); got != test.want {
				t.Fatalf("superseded collection switch %q = %v, want %v", test.value, got, test.want)
			}
		})
	}
}
