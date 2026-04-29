// Package proxy implements the in-cluster TCP proxy that injects
// PROXY-protocol v1 headers in front of forwarded connections.
package proxy

import (
	"fmt"
	"net"
)

const (
	headerUnknown = "PROXY UNKNOWN\r\n"
	familyTCP4    = "TCP4"
	familyTCP6    = "TCP6"
)

// addrInfo is the canonical representation of an endpoint used to assemble
// a PROXY-protocol header. An empty family signals an invalid or unknown
// address.
type addrInfo struct {
	text   string
	family string
}

// V1Header constructs a PROXY-protocol v1 ASCII header line for the given
// source and destination TCP addresses, terminated by CRLF.
//
// Returns "PROXY UNKNOWN\r\n" when either address is not a *net.TCPAddr,
// when either address is a typed-nil pointer, or when source and destination
// belong to different address families. IPv4-mapped IPv6 addresses
// (::ffff:a.b.c.d) are unwrapped to plain IPv4.
func V1Header(src, dst net.Addr) string {
	srcTCP, srcOK := src.(*net.TCPAddr)
	dstTCP, dstOK := dst.(*net.TCPAddr)

	if !srcOK || !dstOK || srcTCP == nil || dstTCP == nil {
		return headerUnknown
	}

	srcInfo := normalize(srcTCP.IP)
	dstInfo := normalize(dstTCP.IP)

	if srcInfo.family == "" || dstInfo.family == "" || srcInfo.family != dstInfo.family {
		return headerUnknown
	}

	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n",
		srcInfo.family, srcInfo.text, dstInfo.text, srcTCP.Port, dstTCP.Port)
}

// normalize returns the canonical text form and PROXY-protocol family token
// for addr. IPv4-mapped IPv6 addresses are unwrapped to TCP4. A zero-value
// addrInfo signals nil or an otherwise invalid address.
func normalize(addr net.IP) addrInfo {
	if addr == nil {
		return addrInfo{}
	}

	if v4 := addr.To4(); v4 != nil {
		return addrInfo{text: v4.String(), family: familyTCP4}
	}

	if v6 := addr.To16(); v6 != nil {
		return addrInfo{text: v6.String(), family: familyTCP6}
	}

	return addrInfo{}
}
