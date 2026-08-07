package ztnet

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coredns/caddy"
	"github.com/coredns/caddy/caddyfile"
	ctest "github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

// ---- helpers ---------------------------------------------------------------

// captureWriter records the message written by the plugin.
type captureWriter struct {
	ctest.ResponseWriter
	msg *dns.Msg
}

func (c *captureWriter) WriteMsg(m *dns.Msg) error {
	c.msg = m
	return nil
}

type stubHandler struct {
	rc int
}

func (s stubHandler) ServeDNS(_ context.Context, _ dns.ResponseWriter, _ *dns.Msg) (int, error) {
	return s.rc, nil
}

func (s stubHandler) Name() string { return "stub" }

// fakeZTNET serves the ZTNET endpoints used by the plugin.
type fakeZTNET struct {
	networks map[string]networkInfo
	members  map[string][]member
}

func (f *fakeZTNET) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) < 2 || parts[0] != "network" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		id := parts[1]
		w.Header().Set("Content-Type", "application/json")
		switch {
		case len(parts) == 2:
			info, ok := f.networks[id]
			if !ok {
				http.Error(w, `{"error":"network not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(info)
		case len(parts) == 3 && parts[2] == "member":
			members, ok := f.members[id]
			if !ok {
				http.Error(w, `{"error":"network not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(members)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	})
}

type v6Mode struct {
	Sixplane bool `json:"6plane"`
	RFC4193  bool `json:"rfc4193"`
}

// testFixture builds a plugin bound to a fake ZTNET server with two networks,
// both published under the example.com. zone.
func testFixture(t *testing.T) (*Ztnet, *fakeZTNET) {
	t.Helper()
	fake := &fakeZTNET{
		networks: map[string]networkInfo{
			"8056c2e21c000001": {ID: "8056c2e21c000001", V6AssignMode: v6Mode{Sixplane: true, RFC4193: true}},
			"0123456789abcdef": {ID: "0123456789abcdef", V6AssignMode: v6Mode{}},
		},
		members: map[string][]member{
			"8056c2e21c000001": {
				{ID: "1234567890abcdef", Name: "server one", Authorized: true, IPAssignments: []string{"10.0.0.1"}},
				{ID: "0011223344556677", Name: "client", Authorized: false, IPAssignments: []string{"10.0.0.2"}},
				{ID: "aabbccddeeff0011", Name: "", Authorized: true, IPAssignments: []string{"10.0.0.3"}},
			},
			"0123456789abcdef": {
				{ID: "0011223344556677", Name: "v6only", Authorized: true, IPAssignments: []string{"fd00::1"}},
			},
		},
	}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	z := &Ztnet{
		networks: []network{
			{zone: "example.com.", networkID: "8056c2e21c000001"},
			{zone: "example.com.", networkID: "0123456789abcdef"},
		},
		zones:    []string{"example.com."},
		dotZones: []string{".example.com."},
		api:      newAPIClient(srv.URL, "test-token", false),
		ttl:      60,
	}
	return z, fake
}

func query(w *captureWriter, z *Ztnet, qname string, qtype uint16) (int, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)
	return z.ServeDNS(context.Background(), w, m)
}

func answerStrings(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		out = append(out, rr.String())
	}
	return out
}

func equalRRSets(t *testing.T, got, want []string) bool {
	t.Helper()
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool)
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// ---- address generation ----------------------------------------------------

// Expected values were produced by running the functions of zt2hosts.sh
// against the same inputs.

func TestRFC4193Address(t *testing.T) {
	tests := []struct {
		nwid, node, want string
	}{
		{"8056c2e21c000001", "1234567890abcdef", "fd80:56c2:e21c:0000:0199:9312:3456:7890"},
		{"0123456789abcdef", "0011223344556677", "fd01:2345:6789:abcd:ef99:9300:1122:3344"},
	}
	for _, tt := range tests {
		if got := rfc4193Address(tt.nwid, tt.node); got != tt.want {
			t.Errorf("rfc4193Address(%s, %s) = %q, want %q", tt.nwid, tt.node, got, tt.want)
		}
	}
}

func TestSixplaneAddress(t *testing.T) {
	tests := []struct {
		nwid, node, want string
	}{
		// Identical to the script's output for this input.
		{"8056c2e21c000001", "1234567890abcdef", "fc8c:56c2:e312:3456:7890:0000:0000:0001"},
		// The script's printf '%x' drops the leading zero (0x08888888 ->
		// "8888888") and the cut -c slices shift the nibble boundaries. The
		// result is a valid but shifted address; the plugin reproduces it
		// byte-for-byte.
		{"0123456789abcdef", "0011223344556677", "fc88:8888:800:1122:3344:0000:0000:0001"},
	}
	for _, tt := range tests {
		if got := sixplaneAddress(tt.nwid, tt.node); got != tt.want {
			t.Errorf("sixplaneAddress(%s, %s) = %q, want %q", tt.nwid, tt.node, got, tt.want)
		}
	}
}

func TestInvalidIDs(t *testing.T) {
	if got := rfc4193Address("short", "1234567890abcdef"); got != "" {
		t.Errorf("rfc4193Address with short nwid = %q, want empty", got)
	}
	if got := sixplaneAddress("zzzzzzzzzzzzzzzz", "1234567890abcdef"); got != "" {
		t.Errorf("sixplaneAddress with non-hex nwid = %q, want empty", got)
	}
}

func TestReverseName(t *testing.T) {
	if got := reverseName(net.ParseIP("10.0.0.1")); got != "1.0.0.10.in-addr.arpa." {
		t.Errorf("reverseName(10.0.0.1) = %q", got)
	}
	if got := reverseName(net.ParseIP("fd80:56c2:e21c:0000:0199:9312:3456:7890")); got != "0.9.8.7.6.5.4.3.2.1.3.9.9.9.1.0.0.0.0.0.c.1.2.e.2.c.6.5.0.8.d.f.ip6.arpa." {
		t.Errorf("reverseName(v6) = %q", got)
	}
}

func TestMatchZone(t *testing.T) {
	zones := []string{"example.com.", "zt.example.com."}
	dotZones := []string{".example.com.", ".zt.example.com."}
	tests := []struct {
		qname, zone, host string
	}{
		{"node.example.com.", "example.com.", "node"},
		{"a.b.example.com.", "example.com.", "a.b"},
		{"node.zt.example.com.", "zt.example.com.", "node"},
		{"example.com.", "example.com.", ""},
		{"other.org.", "", ""},
	}
	for _, tt := range tests {
		zone, host := matchZone(zones, dotZones, tt.qname)
		if zone != tt.zone || host != tt.host {
			t.Errorf("matchZone(%q) = (%q, %q), want (%q, %q)", tt.qname, zone, host, tt.zone, tt.host)
		}
	}
}

// ---- DNS serving -----------------------------------------------------------

func TestServeDNSA(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "server_one.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("query returned (%d, %v)", rc, err)
	}
	want := []string{"server_one.example.com.\t60\tIN\tA\t10.0.0.1"}
	if got := answerStrings(w.msg); !equalRRSets(t, got, want) {
		t.Errorf("A answer = %v, want %v", got, want)
	}
}

func TestServeDNSNodeID(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "1234567890abcdef.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("query returned (%d, %v)", rc, err)
	}
	want := []string{"1234567890abcdef.example.com.\t60\tIN\tA\t10.0.0.1"}
	if got := answerStrings(w.msg); !equalRRSets(t, got, want) {
		t.Errorf("A answer = %v, want %v", got, want)
	}
}

func TestServeDNSAAAA(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "server_one.example.com", dns.TypeAAAA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("query returned (%d, %v)", rc, err)
	}
	want := []string{
		"server_one.example.com.\t60\tIN\tAAAA\tfc8c:56c2:e312:3456:7890::1",
		"server_one.example.com.\t60\tIN\tAAAA\tfd80:56c2:e21c:0:199:9312:3456:7890",
	}
	if got := answerStrings(w.msg); !equalRRSets(t, got, want) {
		t.Errorf("AAAA answer = %v, want %v", got, want)
	}
}

func TestServeDNSPTR(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	for _, tc := range []struct {
		qname, want string
	}{
		{"1.0.0.10.in-addr.arpa.", "server_one.example.com."},
		{"0.9.8.7.6.5.4.3.2.1.3.9.9.9.1.0.0.0.0.0.c.1.2.e.2.c.6.5.0.8.d.f.ip6.arpa.", "server_one.example.com."},
		{"3.0.0.10.in-addr.arpa.", "aabbccddeeff0011.example.com."},
	} {
		w := &captureWriter{}
		rc, err := query(w, z, tc.qname, dns.TypePTR)
		if err != nil || rc != dns.RcodeSuccess {
			t.Fatalf("PTR %q returned (%d, %v)", tc.qname, rc, err)
		}
		if len(w.msg.Answer) != 1 {
			t.Fatalf("PTR %q: got %d answers", tc.qname, len(w.msg.Answer))
		}
		ptr, ok := w.msg.Answer[0].(*dns.PTR)
		if !ok || ptr.Ptr != tc.want {
			t.Errorf("PTR %q = %v, want %q", tc.qname, w.msg.Answer, tc.want)
		}
	}
}

func TestServeDNSPTRForNodeWithoutName(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	// The nameless authorized member still gets PTR records under its node ID.
	w := &captureWriter{}
	rc, err := query(w, z, "3.0.0.10.in-addr.arpa", dns.TypePTR)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("PTR query returned (%d, %v)", rc, err)
	}
	if len(w.msg.Answer) != 1 {
		t.Fatalf("PTR: got %d answers", len(w.msg.Answer))
	}
	if ptr := w.msg.Answer[0].(*dns.PTR); ptr.Ptr != "aabbccddeeff0011.example.com." {
		t.Errorf("PTR = %v", ptr)
	}
}

func TestServeDNSNODATA(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	// v6only has only an IPv6 assignment, so an A query is NODATA.
	w := &captureWriter{}
	rc, err := query(w, z, "v6only.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("query returned (%d, %v)", rc, err)
	}
	if len(w.msg.Answer) != 0 || len(w.msg.Ns) != 1 {
		t.Errorf("NODATA: answer=%v ns=%v", w.msg.Answer, w.msg.Ns)
	}
	if _, ok := w.msg.Ns[0].(*dns.SOA); !ok {
		t.Errorf("NODATA: expected SOA in authority, got %T", w.msg.Ns[0])
	}
}

func TestServeDNSSixplaneOff(t *testing.T) {
	// Network 0123456789abcdef has neither v6 mode enabled: only the assigned
	// IPv6 address should be served, not any generated address.
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "v6only.example.com", dns.TypeAAAA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("query returned (%d, %v)", rc, err)
	}
	want := []string{"v6only.example.com.\t60\tIN\tAAAA\tfd00::1"}
	if got := answerStrings(w.msg); !equalRRSets(t, got, want) {
		t.Errorf("AAAA answer = %v, want %v", got, want)
	}
}

func TestServeDNSNXDOMAIN(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "ghost.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeNameError {
		t.Fatalf("query returned (%d, %v), want NXDOMAIN", rc, err)
	}
	if len(w.msg.Ns) != 1 {
		t.Errorf("NXDOMAIN: expected SOA in authority")
	}
}

func TestServeDNSApex(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "example.com", dns.TypeSOA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("SOA query returned (%d, %v)", rc, err)
	}
	if len(w.msg.Answer) != 1 {
		t.Fatalf("SOA query: got %d answers", len(w.msg.Answer))
	}
	if _, ok := w.msg.Answer[0].(*dns.SOA); !ok {
		t.Errorf("SOA query: expected SOA, got %T", w.msg.Answer[0])
	}
}

func TestServeDNSOutsideZone(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())
	z.Next = stubHandler{rc: dns.RcodeRefused}

	w := &captureWriter{}
	rc, err := query(w, z, "host.other.org", dns.TypeA)
	if err != nil || rc != dns.RcodeRefused {
		t.Fatalf("outside-zone query returned (%d, %v), want REFUSED from next", rc, err)
	}
	if w.msg != nil {
		t.Errorf("outside-zone query must not be answered by ztnet")
	}
}

func TestServeDNSFallthrough(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())
	z.fall = true
	z.Next = stubHandler{rc: dns.RcodeRefused}

	w := &captureWriter{}
	rc, err := query(w, z, "ghost.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeRefused {
		t.Fatalf("fallthrough query returned (%d, %v), want REFUSED from next", rc, err)
	}
	if w.msg != nil {
		t.Errorf("fallthrough query must not be answered by ztnet")
	}
}

func TestServeDNSPTRUnknownFallsThrough(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())
	z.Next = stubHandler{rc: dns.RcodeRefused}

	w := &captureWriter{}
	rc, err := query(w, z, "2.0.0.10.in-addr.arpa", dns.TypePTR)
	if err != nil || rc != dns.RcodeRefused {
		t.Fatalf("unknown PTR returned (%d, %v), want REFUSED from next", rc, err)
	}
}

func TestServeDNSNotReady(t *testing.T) {
	z, _ := testFixture(t)

	w := &captureWriter{}
	rc, err := query(w, z, "server_one.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeServerFailure {
		t.Fatalf("pre-refresh query returned (%d, %v), want SERVFAIL", rc, err)
	}
}

func TestRefreshFailureKeepsOldData(t *testing.T) {
	z, fake := testFixture(t)
	z.refreshData(context.Background())

	// The fake server has no way to be stopped from here, so make requests
	// fail instead by pointing the client at an unroutable address.
	z.api = newAPIClient("http://127.0.0.1:1", "test-token", false)
	z.refreshData(context.Background()) // must fail and keep old data
	_ = fake

	w := &captureWriter{}
	rc, err := query(w, z, "server_one.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("post-failure query returned (%d, %v)", rc, err)
	}
	want := []string{"server_one.example.com.\t60\tIN\tA\t10.0.0.1"}
	if got := answerStrings(w.msg); !equalRRSets(t, got, want) {
		t.Errorf("A answer = %v, want %v", got, want)
	}
}

func TestServeDNSZoneIsolation(t *testing.T) {
	fake := &fakeZTNET{
		networks: map[string]networkInfo{
			"8056c2e21c000001": {ID: "8056c2e21c000001", V6AssignMode: v6Mode{}},
			"0123456789abcdef": {ID: "0123456789abcdef", V6AssignMode: v6Mode{}},
		},
		members: map[string][]member{
			"8056c2e21c000001": {{ID: "1234567890abcdef", Name: "server_one", Authorized: true, IPAssignments: []string{"10.0.0.1"}}},
			"0123456789abcdef": {{ID: "0011223344556677", Name: "other", Authorized: true, IPAssignments: []string{"10.0.0.2"}}},
		},
	}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	z := &Ztnet{
		networks: []network{
			{zone: "a.example.com.", networkID: "8056c2e21c000001"},
			{zone: "b.example.org.", networkID: "0123456789abcdef"},
		},
		zones:    []string{"a.example.com.", "b.example.org."},
		dotZones: []string{".a.example.com.", ".b.example.org."},
		api:      newAPIClient(srv.URL, "test-token", false),
		ttl:      60,
	}
	z.refreshData(context.Background())

	// A member of a.example.com must not resolve under b.example.org.
	for _, qname := range []string{"server_one.b.example.org", "1234567890abcdef.b.example.org"} {
		w := &captureWriter{}
		rc, err := query(w, z, qname, dns.TypeA)
		if err != nil || rc != dns.RcodeNameError {
			t.Errorf("cross-zone query %q returned (%d, %v), want NXDOMAIN", qname, rc, err)
		}
	}
	// ...and still resolves under its own zone.
	w := &captureWriter{}
	rc, err := query(w, z, "server_one.a.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("in-zone query returned (%d, %v)", rc, err)
	}
}

func TestServeDNSApexNODATA(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeMX} {
		w := &captureWriter{}
		rc, err := query(w, z, "example.com", qtype)
		if err != nil || rc != dns.RcodeSuccess {
			t.Fatalf("apex qtype %d returned (%d, %v), want NOERROR", qtype, rc, err)
		}
		if len(w.msg.Answer) != 0 || len(w.msg.Ns) != 1 {
			t.Errorf("apex qtype %d: answer=%v ns=%v", qtype, w.msg.Answer, w.msg.Ns)
		}
		if _, ok := w.msg.Ns[0].(*dns.SOA); !ok {
			t.Errorf("apex qtype %d: expected SOA in authority", qtype)
		}
	}
}

func TestServeDNSSOASubname(t *testing.T) {
	z, _ := testFixture(t)
	z.refreshData(context.Background())

	// Unknown subname: NXDOMAIN, not a bogus SOA answer.
	w := &captureWriter{}
	rc, err := query(w, z, "ghost.example.com", dns.TypeSOA)
	if err != nil || rc != dns.RcodeNameError {
		t.Fatalf("SOA for unknown subname returned (%d, %v), want NXDOMAIN", rc, err)
	}
	// Known subname: NODATA with SOA in authority.
	w2 := &captureWriter{}
	rc, err = query(w2, z, "server_one.example.com", dns.TypeSOA)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("SOA for known subname returned (%d, %v)", rc, err)
	}
	if len(w2.msg.Answer) != 0 || len(w2.msg.Ns) != 1 {
		t.Errorf("SOA known subname: answer=%v ns=%v", w2.msg.Answer, w2.msg.Ns)
	}
}

func TestStartupOutageStaysSERVFAIL(t *testing.T) {
	z, _ := testFixture(t)
	z.api = newAPIClient("http://127.0.0.1:1", "test-token", false)
	z.refreshData(context.Background()) // all fetches fail

	w := &captureWriter{}
	rc, err := query(w, z, "server_one.example.com", dns.TypeA)
	if err != nil || rc != dns.RcodeServerFailure {
		t.Fatalf("post-outage query returned (%d, %v), want SERVFAIL", rc, err)
	}
}

func TestPTRFirstWinsAcrossNetworks(t *testing.T) {
	fake := &fakeZTNET{
		networks: map[string]networkInfo{
			"8056c2e21c000001": {ID: "8056c2e21c000001", V6AssignMode: v6Mode{}},
			"0123456789abcdef": {ID: "0123456789abcdef", V6AssignMode: v6Mode{}},
		},
		members: map[string][]member{
			"8056c2e21c000001": {{ID: "1234567890abcdef", Name: "alpha", Authorized: true, IPAssignments: []string{"10.0.0.1"}}},
			"0123456789abcdef": {{ID: "0011223344556677", Name: "beta", Authorized: true, IPAssignments: []string{"10.0.0.1"}}},
		},
	}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	z := &Ztnet{
		networks: []network{
			{zone: "example.com.", networkID: "8056c2e21c000001"},
			{zone: "example.com.", networkID: "0123456789abcdef"},
		},
		zones:    []string{"example.com."},
		dotZones: []string{".example.com."},
		api:      newAPIClient(srv.URL, "test-token", false),
		ttl:      60,
	}
	z.refreshData(context.Background())

	w := &captureWriter{}
	rc, err := query(w, z, "1.0.0.10.in-addr.arpa", dns.TypePTR)
	if err != nil || rc != dns.RcodeSuccess {
		t.Fatalf("PTR query returned (%d, %v)", rc, err)
	}
	if len(w.msg.Answer) != 1 {
		t.Fatalf("PTR: got %d answers", len(w.msg.Answer))
	}
	if ptr := w.msg.Answer[0].(*dns.PTR); ptr.Ptr != "alpha.example.com." {
		t.Errorf("PTR = %v, want alpha.example.com.", ptr)
	}
}

// ---- Corefile parsing ------------------------------------------------------

func parseCorefile(t *testing.T, input string) (*Ztnet, error) {
	t.Helper()
	sbs, err := caddyfile.Parse("Testfile", strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("caddyfile.Parse: %v", err)
	}
	if len(sbs) == 0 {
		t.Fatalf("no server blocks parsed")
	}
	tokens, ok := sbs[0].Tokens["ztnet"]
	if !ok {
		t.Fatalf("no ztnet directive found")
	}
	c := &caddy.Controller{Dispenser: caddyfile.NewDispenserTokens("Testfile", tokens)}
	return parse(c)
}

func TestParse(t *testing.T) {
	t.Setenv("ZTNET_API_TOKEN", "")
	z, err := parseCorefile(t, `
. {
    ztnet example.com:8056c2e21c000001 example.org:8056c2e21c000002 {
        api http://localhost:3000
        token secret
        refresh 45s
        ttl 5s
        fallthrough
    }
}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(z.networks) != 2 || z.networks[0].zone != "example.com." || z.networks[1].zone != "example.org." {
		t.Errorf("networks = %+v", z.networks)
	}
	if !z.fall || z.ttl != 5 {
		t.Errorf("fall=%v ttl=%d", z.fall, z.ttl)
	}
	if z.refresh != 45_000_000_000 {
		t.Errorf("refresh = %v", z.refresh)
	}
	if z.api.base != "http://localhost:3000/api/v1" {
		t.Errorf("api base = %q", z.api.base)
	}
}

func TestParseNetworkDirective(t *testing.T) {
	t.Setenv("ZTNET_API_TOKEN", "")
	z, err := parseCorefile(t, `
. {
    ztnet {
        network example.com 8056c2e21c000001
        token secret
    }
}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(z.networks) != 1 || z.networks[0].zone != "example.com." {
		t.Errorf("networks = %+v", z.networks)
	}
}

func TestParseTokenFromEnv(t *testing.T) {
	t.Setenv("ZTNET_API_TOKEN", "env-secret")
	z, err := parseCorefile(t, `
. {
    ztnet example.com:8056c2e21c000001
}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if z.api.token != "env-secret" {
		t.Errorf("token = %q, want env-secret", z.api.token)
	}
}

func TestParseTokenInCorefile(t *testing.T) {
	t.Setenv("ZTNET_API_TOKEN", "")
	z, err := parseCorefile(t, `
. {
    ztnet example.com:8056c2e21c000001 {
        token corefile-secret
    }
}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if z.api.token != "corefile-secret" {
		t.Errorf("token = %q, want corefile-secret", z.api.token)
	}
}

func TestParseErrors(t *testing.T) {
	t.Setenv("ZTNET_API_TOKEN", "")
	tests := []struct {
		name  string
		input string
	}{
		{"no networks", `.
	{
		ztnet {
			token secret
		}
	}`},
		{"bad zone", `.
	{
		ztnet not_a_zone:8056c2e21c000001 {
			token secret
		}
	}`},
		{"bad network id", `.
	{
		ztnet example.com:zz {
			token secret
		}
	}`},
		{"missing token", `.
	{
		ztnet example.com:8056c2e21c000001
	}`},
		{"bad refresh", `.
	{
		ztnet example.com:8056c2e21c000001 {
			token secret
			refresh nope
		}
	}`},
		{"zero refresh", `.
	{
		ztnet example.com:8056c2e21c000001 {
			token secret
			refresh 0s
		}
	}`},
		{"unknown property", `.
	{
		ztnet example.com:8056c2e21c000001 {
			token secret
			bogus x
		}
	}`},
	}
	for _, tt := range tests {
		z, err := parseCorefile(t, tt.input)
		if err == nil {
			t.Errorf("%s: expected error, got %+v", tt.name, z)
		}
	}
}
