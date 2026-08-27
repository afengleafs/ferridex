package server

import "testing"

func TestNgrokPublicURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   `msg="started tunnel" url=https://unleaded-spectator-ducking.ngrok-free.dev`,
			want: "https://unleaded-spectator-ducking.ngrok-free.dev",
		},
		{
			in:   `Forwarding https://foo-bar.ngrok-free.app -> http://127.0.0.1:8789`,
			want: "https://foo-bar.ngrok-free.app",
		},
		{
			in:   `url=https://example.ngrok.app`,
			want: "https://example.ngrok.app",
		},
		{
			in:   `url=https://legacy.ngrok.io`,
			want: "https://legacy.ngrok.io",
		},
		{
			in:   `url=http://insecure.ngrok-free.dev`,
			want: "",
		},
		{
			in:   `no tunnel yet`,
			want: "",
		},
	}
	for _, tc := range cases {
		got := ngrokPublicURL.FindString(tc.in)
		if got != tc.want {
			t.Errorf("FindString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
