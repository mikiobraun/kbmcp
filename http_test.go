package main

import "testing"

// clientTag guards a log line against its own input: X-Client-Id is set by the
// caller, so a value carrying a newline could forge a second log line, and one
// carrying the field separators could blur which part of [REST:<ip>:<tag>] is
// which. Anything outside printable ASCII is dropped rather than escaped —
// a client id has no business containing it.
func TestClientTag(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain name kept", "kb-editor", "kb-editor"},
		{"empty stays empty", "", ""},
		{"newline cannot forge a line", "a\nkbmcp: [REST:1.2.3.4] GET /files/", "akbmcp[REST1.2.3.4]GET/files/"},
		{"separator and quote dropped", `evil"] user="root`, "evil]user=root"},
		{"control characters dropped", "kb\x00\x1b[31meditor", "kb[31meditor"},
		{"non-ascii dropped", "kb–editor", "kbeditor"},
		{"capped at 32 characters", "0123456789012345678901234567890123456789", "01234567890123456789012345678901"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientTag(tc.in); got != tc.want {
				t.Errorf("clientTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
