package api

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Camera 0001":    "camera-0001",
		"  Cổng chính  ": "c-ng-ch-nh",
		"cam_01.live":    "cam_01.live",
		"a  b   c":       "a-b-c",
		"###":            "",
		"UPPER--CASE":    "upper-case",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugPattern(t *testing.T) {
	valid := []string{"vcam-0001", "cam_01", "a.b", "x"}
	invalid := []string{"", "-cam", "Cam-1", "cam 1", "cam/1"}
	for _, s := range valid {
		if !slugRe.MatchString(s) {
			t.Errorf("%q should be a valid slug", s)
		}
	}
	for _, s := range invalid {
		if slugRe.MatchString(s) {
			t.Errorf("%q should be an invalid slug", s)
		}
	}
}
