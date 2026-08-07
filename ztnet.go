package ztnet

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

// network couples a DNS zone with the ZeroTier network whose authorized
// members are published under that zone. It corresponds to the
// "DNSNAME:NETWORK" arguments of zt2hosts.sh.
type network struct {
	zone      string // lowercase FQDN with trailing dot
	networkID string
}

// zoneData holds the records generated for a single network.
type zoneData struct {
	v4  map[string][]net.IP
	v6  map[string][]net.IP
	ptr map[string]string
}

// snapshot is an immutable copy of the served records. It is swapped
// atomically on every successful refresh, so query handling never blocks.
type snapshot struct {
	netData map[string]*zoneData // keyed by network ID; carries data across refreshes
	v4      map[string][]net.IP  // keyed by zoneKey(zone, host)
	v6      map[string][]net.IP  // keyed by zoneKey(zone, host)
	ptr     map[string]string
	serial  uint32
}

// zoneKey namespaces a host name within its zone so that records never leak
// across the configured zones.
func zoneKey(zone, host string) string {
	return zone + "\x00" + host
}

// Ztnet is a CoreDNS plugin that serves A, AAAA and PTR records generated
// from authorized members of ZeroTier networks, fetched from a ZTNET server.
type Ztnet struct {
	Next     plugin.Handler
	networks []network
	zones    []string
	dotZones []string // "."+zones[i], precomputed for the query hot path
	api      *apiClient
	refresh  time.Duration
	ttl      uint32
	fall     bool

	snap  atomic.Pointer[snapshot]
	ready atomic.Bool
	stopc chan struct{}
	wg    sync.WaitGroup
}

// Name implements plugin.Handler.
func (z *Ztnet) Name() string { return "ztnet" }

// ServeDNS implements plugin.Handler.
func (z *Ztnet) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.Name()

	// Reverse queries are answered only when we hold a PTR record; everything
	// else is left to the next plugin.
	if state.QType() == dns.TypePTR {
		if snap := z.snap.Load(); z.ready.Load() && snap != nil {
			if target, ok := snap.ptr[qname]; ok {
				m := new(dns.Msg)
				m.SetReply(r)
				m.Authoritative = true
				m.Answer = []dns.RR{&dns.PTR{
					Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: z.ttl},
					Ptr: target,
				}}
				w.WriteMsg(m)
				return dns.RcodeSuccess, nil
			}
		}
		return plugin.NextOrFailure(z.Name(), z.Next, ctx, w, r)
	}

	zone, host := matchZone(z.zones, z.dotZones, qname)
	if zone == "" {
		return plugin.NextOrFailure(z.Name(), z.Next, ctx, w, r)
	}
	snap := z.snap.Load()
	if !z.ready.Load() || snap == nil {
		// We own the zone but no data has been fetched yet.
		return dns.RcodeServerFailure, nil
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	// The zone apex exists (a SOA is served for it); every other type at the
	// apex is NODATA, never NXDOMAIN.
	if host == "" {
		if state.QType() == dns.TypeSOA {
			m.Answer = []dns.RR{z.soa(zone, snap.serial)}
		} else {
			m.Ns = []dns.RR{z.soa(zone, snap.serial)}
		}
		w.WriteMsg(m)
		return dns.RcodeSuccess, nil
	}

	key := zoneKey(zone, host)
	v4 := snap.v4[key]
	v6 := snap.v6[key]
	exists := len(v4) > 0 || len(v6) > 0

	switch state.QType() {
	case dns.TypeSOA:
		if !exists {
			return z.noSuchName(ctx, w, r, zone, snap.serial)
		}
		m.Ns = []dns.RR{z.soa(zone, snap.serial)} // NODATA
	case dns.TypeA:
		if len(v4) > 0 {
			for _, ip := range v4 {
				hdr := dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: z.ttl}
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: ip.To4()})
			}
		} else if !exists {
			return z.noSuchName(ctx, w, r, zone, snap.serial)
		} else {
			m.Ns = []dns.RR{z.soa(zone, snap.serial)} // NODATA
		}
	case dns.TypeAAAA:
		if len(v6) > 0 {
			for _, ip := range v6 {
				hdr := dns.RR_Header{Name: qname, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: z.ttl}
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: ip.To16()})
			}
		} else if !exists {
			return z.noSuchName(ctx, w, r, zone, snap.serial)
		} else {
			m.Ns = []dns.RR{z.soa(zone, snap.serial)} // NODATA
		}
	default:
		if !exists {
			return z.noSuchName(ctx, w, r, zone, snap.serial)
		}
		m.Ns = []dns.RR{z.soa(zone, snap.serial)} // NODATA for other types
	}

	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

// noSuchName replies NXDOMAIN unless fallthrough is enabled.
func (z *Ztnet) noSuchName(ctx context.Context, w dns.ResponseWriter, r *dns.Msg, zone string, serial uint32) (int, error) {
	if z.fall {
		return plugin.NextOrFailure(z.Name(), z.Next, ctx, w, r)
	}
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeNameError)
	m.Authoritative = true
	m.Ns = []dns.RR{z.soa(zone, serial)}
	w.WriteMsg(m)
	return dns.RcodeNameError, nil
}

// soa synthesizes a SOA record for the zone.
func (z *Ztnet) soa(zone string, serial uint32) *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: z.ttl},
		Ns:      "ns." + zone,
		Mbox:    "hostmaster." + zone,
		Serial:  serial,
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  z.ttl,
	}
}

// matchZone returns the longest configured zone that qname belongs to, plus
// the left-most label(s) (host) below it. zone is "" when qname is outside
// every configured zone. dotZones[i] is "."+zones[i], precomputed to avoid
// per-query allocations.
func matchZone(zones, dotZones []string, qname string) (zone, host string) {
	best := -1
	for i, zn := range zones {
		if qname == zn {
			best = len(zn)
			zone, host = zn, ""
			continue
		}
		if strings.HasSuffix(qname, dotZones[i]) && len(zn) > best {
			best = len(zn)
			zone = zn
			host = qname[:len(qname)-len(zn)-1]
		}
	}
	return zone, host
}

// start launches the background refresh loop. The first refresh happens
// immediately; subsequent ones follow the configured interval.
func (z *Ztnet) start() {
	z.stopc = make(chan struct{})
	z.wg.Add(1)
	go func() {
		defer z.wg.Done()
		z.refreshData(context.Background())
		ticker := time.NewTicker(z.refresh)
		defer ticker.Stop()
		for {
			select {
			case <-z.stopc:
				return
			case <-ticker.C:
				z.refreshData(context.Background())
			}
		}
	}()
}

// stop terminates the refresh loop and waits for it to finish.
func (z *Ztnet) stop() {
	if z.stopc == nil {
		return
	}
	close(z.stopc)
	z.wg.Wait()
}

// refreshData fetches every configured network from ZTNET and atomically swaps
// the served records. Networks that fail to fetch keep their previous data.
// If every fetch fails the previous snapshot is left untouched (and the plugin
// stays not-ready when it has never succeeded).
func (z *Ztnet) refreshData(ctx context.Context) {
	var (
		mu      sync.Mutex
		netData map[string]*zoneData
		okCount int
	)
	if old := z.snap.Load(); old != nil {
		netData = make(map[string]*zoneData, len(old.netData))
		for k, v := range old.netData {
			netData[k] = v
		}
	} else {
		netData = make(map[string]*zoneData)
	}

	var wg sync.WaitGroup
	for _, nw := range z.networks {
		wg.Add(1)
		go func(nw network) {
			defer wg.Done()
			info, err := z.api.network(ctx, nw.networkID)
			if err != nil {
				log.Warningf("ztnet: refresh %s (%s): %v (keeping previous data)", nw.zone, nw.networkID, err)
				return
			}
			members, err := z.api.members(ctx, nw.networkID)
			if err != nil {
				log.Warningf("ztnet: refresh %s (%s): %v (keeping previous data)", nw.zone, nw.networkID, err)
				return
			}
			mu.Lock()
			netData[nw.networkID] = buildZoneData(nw.zone, info, members)
			okCount++
			mu.Unlock()
		}(nw)
	}
	wg.Wait()

	if okCount == 0 {
		log.Warningf("ztnet: all network fetches failed; keeping previous data")
		return
	}

	snap := &snapshot{
		netData: netData,
		v4:      make(map[string][]net.IP),
		v6:      make(map[string][]net.IP),
		ptr:     make(map[string]string),
		serial:  uint32(time.Now().Unix()),
	}
	// Merge in configured order so that when the same IP is announced by
	// several networks, the PTR target is deterministic (first configured
	// network wins), matching the first-wins rule inside buildZoneData.
	for _, nw := range z.networks {
		zd, ok := netData[nw.networkID]
		if !ok {
			continue
		}
		for host, ips := range zd.v4 {
			key := zoneKey(nw.zone, host)
			snap.v4[key] = append(snap.v4[key], ips...)
		}
		for host, ips := range zd.v6 {
			key := zoneKey(nw.zone, host)
			snap.v6[key] = append(snap.v6[key], ips...)
		}
		for name, target := range zd.ptr {
			if _, ok := snap.ptr[name]; !ok {
				snap.ptr[name] = target
			}
		}
	}
	z.snap.Store(snap)
	z.ready.Store(true)
}

// buildZoneData converts an authorized member list into DNS records for one
// zone, mirroring the loop of zt2hosts.sh: each member's IP assignments are
// published under both "name.zone" and "nodeid.zone", and when the network
// enables them, the RFC4193 and 6plane addresses are generated the same way
// the script does.
func buildZoneData(zone string, info networkInfo, members []member) *zoneData {
	zd := &zoneData{
		v4:  make(map[string][]net.IP),
		v6:  make(map[string][]net.IP),
		ptr: make(map[string]string),
	}
	add := func(host string, ip net.IP) {
		if host == "" || ip == nil {
			return
		}
		host = strings.ToLower(host)
		if v4 := ip.To4(); v4 != nil {
			zd.v4[host] = append(zd.v4[host], v4)
		} else {
			zd.v6[host] = append(zd.v6[host], ip.To16())
		}
		// The member name is the primary PTR target; the node ID alias must
		// not overwrite it.
		if _, ok := zd.ptr[reverseName(ip)]; !ok {
			zd.ptr[reverseName(ip)] = host + "." + zone
		}
	}

	for _, m := range members {
		if !m.Authorized {
			continue
		}
		name := sanitizeName(m.Name)
		for _, a := range m.IPAssignments {
			if ip := net.ParseIP(strings.TrimSpace(a)); ip != nil {
				add(name, ip)
				add(m.ID, ip)
			}
		}
		if info.V6AssignMode.RFC4193 {
			if a := rfc4193Address(info.ID, m.ID); a != "" {
				if ip := net.ParseIP(a); ip != nil {
					add(name, ip)
					add(m.ID, ip)
				}
			}
		}
		if info.V6AssignMode.Sixplane {
			if a := sixplaneAddress(info.ID, m.ID); a != "" {
				if ip := net.ParseIP(a); ip != nil {
					add(name, ip)
					add(m.ID, ip)
				}
			}
		}
	}
	return zd
}

// sanitizeName applies the script's name mangling (gsub(" "; "_")) to a
// member name, extending it to any whitespace so the resulting label stays
// valid in DNS.
func sanitizeName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
}

// reverseName returns the DNS reverse lookup name for an IP address.
func reverseName(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", v4[3], v4[2], v4[1], v4[0])
	}
	v6 := ip.To16()
	var sb strings.Builder
	sb.Grow(74)
	for i := len(v6) - 1; i >= 0; i-- {
		fmt.Fprintf(&sb, "%x.%x.", v6[i]&0x0f, v6[i]>>4)
	}
	sb.WriteString("ip6.arpa.")
	return sb.String()
}
