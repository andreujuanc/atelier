package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
)

// privateRanges are the address ranges a network's subnet must lie in: the
// private IPv4 ones and the locally assigned IPv6 ones.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fd00::/8"),
}

// ipamConfigFields are the settings of an entry of a network's IPAM.Config.
var ipamConfigFields = []string{"Subnet", "IPRange", "Gateway", "AuxiliaryAddresses"}

// checkIPAM checks a network create's IPAM: the default driver, no options,
// and subnets (with the addresses in them) that are private (privateRanges)
// and overlap no route of the host but a default one (or a /1). A bridge network's
// subnet becomes a route of the host, to the bridge: one of public
// addresses (a resolver's), or one inside a route the host has (its LAN's,
// its gateway's, a VPN's: the more specific route wins), would send the
// host's own traffic, and every container's, to an address a container of
// the task takes. Without a subnet the daemon picks one that is free.
func (g *Gate) checkIPAM(v any) error {
	ipam, err := as[map[string]any]("IPAM", v)
	if err != nil {
		return err
	}
	if err := fieldKeys(ipam, "Driver", "Config", "Options"); err != nil {
		return err
	}
	for k, v := range ipam {
		if !oneOf(k, "Driver", "Config", "Options") && !isZero(v) {
			return fmt.Errorf("IPAM setting %s is not allowed", k)
		}
	}
	if d, err := as[string]("IPAM.Driver", ipam["Driver"]); err != nil || !oneOf(d, "", "default") {
		return errors.New("only the default IPAM driver is allowed")
	}
	if !isZero(ipam["Options"]) {
		return errors.New("IPAM options are not allowed")
	}
	configs, err := as[[]any]("IPAM.Config", ipam["Config"])
	if err != nil {
		return err
	}
	var routes []netip.Prefix
	for _, c := range configs {
		entry, ok := c.(map[string]any)
		if !ok && c != nil {
			return errors.New("setting IPAM.Config has the wrong type")
		}
		if err := fieldKeys(entry, ipamConfigFields...); err != nil {
			return err
		}
		for k, v := range entry {
			if !slices.Contains(ipamConfigFields, k) && !isZero(v) {
				return fmt.Errorf("IPAM setting %s is not allowed", k)
			}
		}
		s, err := as[string]("IPAM.Config.Subnet", entry["Subnet"])
		if err != nil {
			return err
		}
		if s == "" {
			if slices.ContainsFunc(ipamConfigFields, func(k string) bool { return !isZero(entry[k]) }) {
				return errors.New("network addresses need a subnet")
			}
			continue
		}
		subnet, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("unreadable subnet %q", s)
		}
		subnet = subnet.Masked()
		if !slices.ContainsFunc(privateRanges, func(r netip.Prefix) bool { return inPrefix(subnet, r) }) {
			return fmt.Errorf("subnet %s is not private (it must be in %s)", subnet, joinPrefixes(privateRanges))
		}
		if routes == nil {
			hostRoutes := g.HostRoutes
			if hostRoutes == nil {
				hostRoutes = readHostRoutes
			}
			if routes, err = hostRoutes(); err != nil {
				return fmt.Errorf("subnet %s: cannot read the host's routes to check it: %v", subnet, err)
			}
		}
		for _, r := range routes {
			// A default route (or half of one: a VPN's two /1 routes, as
			// OpenVPN's def1 sets them) is no route to a network.
			if r.Bits() > 1 && r.Overlaps(subnet) {
				return fmt.Errorf("subnet %s overlaps the host's route to %s: leave the subnet out and Docker picks a free one", subnet, r)
			}
		}
		if err := inSubnet(subnet, entry); err != nil {
			return err
		}
	}
	return nil
}

// inSubnet checks that the addresses of an IPAM.Config entry (IPRange,
// Gateway, AuxiliaryAddresses) lie in its subnet.
func inSubnet(subnet netip.Prefix, entry map[string]any) error {
	if s, err := as[string]("IPAM.Config.IPRange", entry["IPRange"]); err != nil {
		return err
	} else if s != "" {
		r, err := netip.ParsePrefix(s)
		if err != nil || !inPrefix(r.Masked(), subnet) {
			return fmt.Errorf("IP range %s is not in subnet %s", s, subnet)
		}
	}
	addrs := map[string]any{"Gateway": entry["Gateway"]}
	aux, err := as[map[string]any]("IPAM.Config.AuxiliaryAddresses", entry["AuxiliaryAddresses"])
	if err != nil {
		return err
	}
	for k, v := range aux {
		addrs["AuxiliaryAddresses "+k] = v
	}
	for name, v := range addrs {
		s, err := as[string]("IPAM.Config."+name, v)
		if err != nil {
			return err
		}
		if s == "" {
			continue
		}
		if a, err := netip.ParseAddr(s); err != nil || !subnet.Contains(a) {
			return fmt.Errorf("address %s is not in subnet %s", s, subnet)
		}
	}
	return nil
}

// inPrefix reports whether p lies in r.
func inPrefix(p, r netip.Prefix) bool { return r.Bits() <= p.Bits() && r.Contains(p.Addr()) }

func joinPrefixes(ps []netip.Prefix) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.String())
	}
	return strings.Join(s, ", ")
}

// readHostRoutes reads the routes of the main table of where the gate runs
// from /proc (IPv6 ones only if the host has IPv6).
func readHostRoutes() ([]netip.Prefix, error) {
	v4, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, err
	}
	v6, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return parseRoutes(string(v4), string(v6))
}

// parseRoutes reads the destinations of /proc/net/route (v4: a header line,
// then destination and mask as little-endian hex in the 2nd and 8th
// columns) and /proc/net/ipv6_route (v6: destination as hex and its prefix
// length in hex in the first two).
func parseRoutes(v4, v6 string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	bad := func(line string) error { return fmt.Errorf("unreadable route %q", line) }
	for i, line := range strings.Split(v4, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) == 0 {
			continue
		}
		if len(f) < 8 {
			return nil, bad(line)
		}
		dst, err1 := strconv.ParseUint(f[1], 16, 32)
		mask, err2 := strconv.ParseUint(f[7], 16, 32)
		if err1 != nil || err2 != nil {
			return nil, bad(line)
		}
		// The kernel prints the addresses as numbers in the host's byte
		// order.
		var a [4]byte
		binary.NativeEndian.PutUint32(a[:], uint32(dst))
		out = append(out, netip.PrefixFrom(netip.AddrFrom4(a), bits.OnesCount32(uint32(mask))).Masked())
	}
	for _, line := range strings.Split(v6, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 2 || len(f[0]) != 32 {
			return nil, bad(line)
		}
		n, err := strconv.ParseUint(f[1], 16, 8)
		if err != nil || n > 128 {
			return nil, bad(line)
		}
		var b [16]byte
		for i := range b {
			v, err := strconv.ParseUint(f[0][2*i:2*i+2], 16, 8)
			if err != nil {
				return nil, bad(line)
			}
			b[i] = byte(v)
		}
		out = append(out, netip.PrefixFrom(netip.AddrFrom16(b), int(n)).Masked())
	}
	return out, nil
}
