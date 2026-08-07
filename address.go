package ztnet

import (
	"fmt"
	"strconv"
)

// The address construction below mirrors the helpers in zt2hosts.sh
// (print_ipv6_id, print_rfc4193, print_6plane) so that a deployment moving
// from the script to this plugin serves byte-identical records.

// ipv6ID mirrors print_ipv6_id(): node[0:2]:node[2:6]:node[6:10]
func ipv6ID(node string) string {
	return node[0:2] + ":" + node[2:6] + ":" + node[6:10]
}

// rfc4193Address mirrors print_rfc4193():
//
//	fd{nwid[0:2]}:{nwid[2:6]}:{nwid[6:10]}:{nwid[10:14]}:{nwid[14:16]}99:93{node[0:2]}:{node[2:6]}:{node[6:10]}
//
// i.e. fd + the full 64-bit network ID with the ZeroTier "99:93" magic bytes,
// then the upper 40 bits of the 64-bit node ID.
func rfc4193Address(nwid, node string) string {
	if !validID(nwid) || !validID(node) {
		return ""
	}
	return fmt.Sprintf("fd%s:%s:%s:%s:%s99:93%s",
		nwid[0:2], nwid[2:6], nwid[6:10], nwid[10:14], nwid[14:16], ipv6ID(node))
}

// sixplaneAddress mirrors print_6plane():
//
//	fc{hash[0:2]}:{hash[2:6]}:{hash[6:8]}{node[0:2]}:{node[2:6]}:{node[6:10]}:0000:0000:0001
//
// where hash is the bitwise XOR of the upper 32 bits (nwid[0:8]) and the tail
// (nwid[9:16]) of the network ID. The script feeds that XOR through
// `printf '%x'` (which drops leading zeroes) and then slices the result with
// cut -c1-2 / -c3-6 / -c7-8; when the XOR has fewer than 8 hex digits those
// cuts shift the nibble boundaries instead of padding. This implementation
// reproduces that exact behaviour (e.g. hash "8888888" yields the hextets
// "88", "8888", "8"), so the generated address is byte-identical to the
// script's output in every case.
func sixplaneAddress(nwid, node string) string {
	if !validID(nwid) || !validID(node) {
		return ""
	}
	top, err1 := strconv.ParseUint(nwid[0:8], 16, 32)
	bot, err2 := strconv.ParseUint(nwid[9:16], 16, 32)
	if err1 != nil || err2 != nil {
		return ""
	}
	hash := strconv.FormatUint(top^bot, 16)
	return fmt.Sprintf("fc%s:%s:%s%s:0000:0000:0001",
		cut(hash, 0, 2), cut(hash, 2, 4), cut(hash, 6, 2), ipv6ID(node))
}

// cut mimics `cut -c lo-hi` (1-indexed, inclusive) applied to s: it returns
// the available characters, or "" when the range starts past the end.
func cut(s string, lo, n int) string {
	if lo >= len(s) {
		return ""
	}
	end := lo + n
	if end > len(s) {
		end = len(s)
	}
	return s[lo:end]
}

// validID reports whether s looks like a ZeroTier network or node ID, i.e.
// exactly 16 hexadecimal digits, as used throughout zt2hosts.sh.
func validID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
