package timeweb

import "testing"

func TestExtractMainIPv4(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		srv  *Server
		want string
	}{
		{
			name: "nil",
			srv:  nil,
			want: "",
		},
		{
			name: "is_main wins",
			srv: &Server{
				Networks: []ServerNetwork{{
					Type: "public",
					Ips: []ServerIP{
						{IP: "10.0.0.1", IsMain: false},
						{IP: "92.255.79.89", IsMain: true},
					},
				}},
			},
			want: "92.255.79.89",
		},
		{
			name: "skip floating pick smallest",
			srv: &Server{
				Networks: []ServerNetwork{{
					Type: "public",
					Ips: []ServerIP{
						{IP: "10.1.1.1", IsFloating: true},
						{IP: "92.255.79.100", IsMain: false},
						{IP: "92.255.79.89", IsMain: false},
					},
				}},
			},
			want: "92.255.79.89",
		},
		{
			name: "type hint floating",
			srv: &Server{
				Networks: []ServerNetwork{{
					Type: "public",
					Ips: []ServerIP{
						{IP: "10.0.0.1", Type: "floating"},
						{IP: "192.0.2.1", Type: "ipv4"},
					},
				}},
			},
			want: "192.0.2.1",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractMainIPv4(tc.srv); got != tc.want {
				t.Fatalf("ExtractMainIPv4() = %q, want %q", got, tc.want)
			}
		})
	}
}
