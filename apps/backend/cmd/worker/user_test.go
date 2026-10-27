package main

import (
	"strings"
	"testing"
)

func TestReadPasswordLineKeepsInnerSpacesAndStripsNewline(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"correct horse battery\n", "correct horse battery"},
		{"  leading and trailing  \r\n", "  leading and trailing  "},
		{"no newline", "no newline"},
	} {
		got, err := readPasswordLine(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}
