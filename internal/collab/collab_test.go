package collab

import "testing"

func TestExtract(t *testing.T) {
	const room = "mgAYTZwEnpRQtca0CTgn-Q"
	const key = "gdJUbTovD94ofDaa8YvhY0-ty16w4fn8PgB6PLnoA30"
	want := room + "." + key

	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "join text plain",
			in:   `omp join "` + room + "." + key + `"`,
			want: want, ok: true,
		},
		{
			name: "join text with ansi colour",
			in:   "\x1b[1;32momp join\x1b[0m \"" + room + "." + key + "\"",
			want: want, ok: true,
		},
		{
			name: "legacy hash form normalized",
			in:   `omp join "` + room + "#" + key + `"`,
			want: want, ok: true,
		},
		{
			name: "browser url bare host",
			in:   "or any web browser: my.omp.sh/#" + room + "." + key,
			want: want, ok: true,
		},
		{
			name: "osc-8 hyperlink target",
			in:   "\x1b]8;;https://my.omp.sh/#" + room + "." + key + "\x1b\\click\x1b]8;;\x1b\\",
			want: want, ok: true,
		},
		{
			name: "custom relay path form",
			in:   "https://relay.example.com:7475/r/foo not-a-link, join: relay.example.com/#" + room + "." + key,
			want: want, ok: true,
		},
		{
			name: "no link present",
			in:   "just some normal omp output about reading files",
			want: "", ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Extract([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v (got %q)", ok, tc.ok, got)
			}
			if got != tc.want {
				t.Fatalf("link=%q want %q", got, tc.want)
			}
		})
	}
}

func TestExtractSplitAcrossReads(t *testing.T) {
	// The detector accumulates bytes, so a link split across two reads must be
	// found once the buffer holds the whole thing.
	full := `prefix omp join "roomAAAAAA.keykeykeykeykeykeykeyZZ"` + " suffix"
	if _, ok := Extract([]byte(full[:30])); ok {
		t.Fatal("should not match a truncated link")
	}
	got, ok := Extract([]byte(full))
	if !ok || got != "roomAAAAAA.keykeykeykeykeykeykeyZZ" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestBrowserURL(t *testing.T) {
	link := "room.key"
	cases := []struct {
		name          string
		relay, webURL string
		want          string
	}{
		{"default", "", "", "https://my.omp.sh/#room.key"},
		{"web url wins", "wss://relay.x", "https://web.x/collab", "https://web.x/collab/#room.key"},
		{"wss relay origin", "wss://relay.example:7475", "", "https://relay.example:7475/#room.key"},
		{"ws localhost relay", "ws://localhost:7475", "", "http://localhost:7475/#room.key"},
		{"relay with path stripped", "wss://relay.example/r/room", "", "https://relay.example/#room.key"},
		{"bare host relay", "relay.example", "", "https://relay.example/#room.key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BrowserURL(link, tc.relay, tc.webURL); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if got := BrowserURL("", "wss://x", ""); got != "" {
		t.Fatalf("empty link should give empty url, got %q", got)
	}
}

func TestRoomID(t *testing.T) {
	if got := RoomID("abc.def"); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := RoomID("noseparator"); got != "noseparator" {
		t.Fatalf("got %q", got)
	}
}
