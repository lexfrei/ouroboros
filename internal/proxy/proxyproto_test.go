package proxy_test

import (
	"net"
	"testing"

	"github.com/lexfrei/ouroboros/internal/proxy"
)

const (
	ipv4Src       = "192.0.2.1"
	ipv4Dst       = "198.51.100.2"
	ipv6Src       = "2001:db8::1"
	ipv6Dst       = "2001:db8::2"
	ipv4MappedSrc = "::ffff:192.0.2.1"
	portSrc       = 12345
	portDst       = 443
)

func TestV1Header(t *testing.T) {
	t.Parallel()

	tcpAddr := func(addr string, port int) *net.TCPAddr {
		return &net.TCPAddr{IP: net.ParseIP(addr), Port: port}
	}
	udpAddr := func(addr string, port int) *net.UDPAddr {
		return &net.UDPAddr{IP: net.ParseIP(addr), Port: port}
	}

	cases := []struct {
		name string
		src  net.Addr
		dst  net.Addr
		want string
	}{
		{
			name: "ipv4_pair",
			src:  tcpAddr(ipv4Src, portSrc),
			dst:  tcpAddr(ipv4Dst, portDst),
			want: "PROXY TCP4 192.0.2.1 198.51.100.2 12345 443\r\n",
		},
		{
			name: "ipv6_pair",
			src:  tcpAddr(ipv6Src, portSrc),
			dst:  tcpAddr(ipv6Dst, portDst),
			want: "PROXY TCP6 2001:db8::1 2001:db8::2 12345 443\r\n",
		},
		{
			name: "ipv4_mapped_unwraps_to_tcp4",
			src:  tcpAddr(ipv4MappedSrc, portSrc),
			dst:  tcpAddr(ipv4Dst, portDst),
			want: "PROXY TCP4 192.0.2.1 198.51.100.2 12345 443\r\n",
		},
		{
			name: "non_tcp_returns_unknown",
			src:  udpAddr(ipv4Src, portSrc),
			dst:  udpAddr(ipv4Dst, portDst),
			want: "PROXY UNKNOWN\r\n",
		},
		{
			name: "mixed_family_returns_unknown",
			src:  tcpAddr(ipv4Src, portSrc),
			dst:  tcpAddr(ipv6Dst, portDst),
			want: "PROXY UNKNOWN\r\n",
		},
		{
			name: "nil_addrs_return_unknown",
			src:  nil,
			dst:  nil,
			want: "PROXY UNKNOWN\r\n",
		},
		{
			name: "typed_nil_tcp_returns_unknown",
			src:  (*net.TCPAddr)(nil),
			dst:  (*net.TCPAddr)(nil),
			want: "PROXY UNKNOWN\r\n",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := proxy.V1Header(tt.src, tt.dst)
			if got != tt.want {
				t.Errorf("V1Header(%v, %v) = %q, want %q", tt.src, tt.dst, got, tt.want)
			}
		})
	}
}
