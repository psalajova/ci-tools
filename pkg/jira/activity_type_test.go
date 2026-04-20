package jira

import "testing"

func TestIsValidActivityType(t *testing.T) {
	for _, v := range AllowedActivityTypes {
		if !IsValidActivityType(v) {
			t.Errorf("expected valid: %q", v)
		}
	}
	if IsValidActivityType("") {
		t.Error("empty string should be invalid")
	}
	if IsValidActivityType("Not an option") {
		t.Error("unknown label should be invalid")
	}
}
