package main

import (
	"strings"
	"testing"
)

func TestJoinCodeRoundTrip(t *testing.T) {
	invitation, hubAddr, err := parseJoinCode(formatJoinCode("abcd0123", "tcexampleaddress"))
	if err != nil {
		t.Fatalf("parseJoinCode: %v", err)
	}
	if invitation != "abcd0123" || hubAddr != "tcexampleaddress" {
		t.Fatalf("parsed = %q, %q", invitation, hubAddr)
	}
}

func TestParseJoinCodeRejections(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"abcd0123:preauthkey0123456789", "retired <invitation>:<preauth-key> format"},
		{"ohya2.abcd0123.tcexampleaddress", "starts with ohya1."},
		{"plaincode", "starts with ohya1."},
		{"ohya1.", "ohya1.<invitation>.<tailcat-address>"},
		{"ohya1.onlyinvitation", "ohya1.<invitation>.<tailcat-address>"},
		{"ohya1..tcexampleaddress", "ohya1.<invitation>.<tailcat-address>"},
		{"ohya1.inv.with.dots", "not a tailcat address"},
		{"ohya1.abcd0123.http://hub:9417", "not a tailcat address"},
		{"ohya1.abcd0123.localhost:9027", "not a tailcat address"},
	}
	for _, tc := range cases {
		_, _, err := parseJoinCode(tc.code)
		if err == nil {
			t.Fatalf("parseJoinCode(%q) accepted the code", tc.code)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("parseJoinCode(%q) err = %v; want it to name %q", tc.code, err, tc.want)
		}
	}
}
