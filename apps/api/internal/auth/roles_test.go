package auth

import "testing"

func TestAllows(t *testing.T) {
	for _, tc := range []struct {
		roles []string
		need  Role
		want  bool
	}{
		{[]string{"viewer"}, Viewer, true},
		{[]string{"viewer"}, Operator, false},
		{[]string{"operator"}, Viewer, true},
		{[]string{"operator"}, Admin, false},
		{[]string{"viewer", "admin"}, Admin, true},
		{[]string{"admin"}, Operator, true},
		{nil, Viewer, false},
		{[]string{"owner"}, Viewer, false},
		{[]string{"admin"}, Role("owner"), false},
	} {
		if got := Allows(tc.roles, tc.need); got != tc.want {
			t.Errorf("Allows(%v, %s) = %t, want %t", tc.roles, tc.need, got, tc.want)
		}
	}
}
