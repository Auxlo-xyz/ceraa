package telegram

import "testing"

func TestGroupDigits(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{27431, "27,431"},
		{3000617, "3,000,617"},
		{10931, "10,931"},
		{1234567, "1,234,567"},
		{1000000, "1,000,000"},
		{999999999, "999,999,999"},
		{-27431, "-27,431"},
	} {
		if got := groupDigits(tc.in); got != tc.want {
			t.Errorf("groupDigits(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
