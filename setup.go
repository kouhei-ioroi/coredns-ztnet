package ztnet

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
)

const (
	defaultAPIAddress = "http://localhost:3000"
	defaultRefresh    = 30 * time.Second
	defaultTTL        = 60
)

// zonePattern mirrors is_valid_zone() in zt2hosts.sh.
var zonePattern = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,6}$`)

// networkIDPattern: ZeroTier network IDs are 64-bit hexadecimal values.
var networkIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)

func init() {
	plugin.Register("ztnet", setup)
}

func setup(c *caddy.Controller) error {
	z, err := parse(c)
	if err != nil {
		return plugin.Error("ztnet", err)
	}
	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		z.Next = next
		return z
	})
	c.OnStartup(func() error {
		z.start()
		return nil
	})
	c.OnShutdown(func() error {
		z.stop()
		return nil
	})
	return nil
}

// parse reads the ztnet directive from the Corefile. It accepts
// "zone:networkID" pairs as positional arguments, mirroring the arguments of
// zt2hosts.sh, plus a number of block properties:
//
//	ztnet example.com:8056c2e21c000001 {
//	    api http://localhost:3000
//	    token <token or ZTNET_API_TOKEN>
//	    refresh 30s
//	    ttl 60s
//	    network other.example.org 8056c2e21c000002
//	    fallthrough
//	    insecure_skip_verify
//	}
func parse(c *caddy.Controller) (*Ztnet, error) {
	z := &Ztnet{refresh: defaultRefresh, ttl: defaultTTL}
	api := defaultAPIAddress
	token := ""
	var networks []network
	insecure := false

	for c.Next() {
		for _, a := range c.RemainingArgs() {
			nw, err := parseNetworkArg(a)
			if err != nil {
				return nil, c.Errf("%v", err)
			}
			networks = append(networks, nw)
		}
		for c.NextBlock() {
			switch c.Val() {
			case "api":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				api = c.Val()
			case "token":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				token = c.Val()
			case "refresh":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid refresh duration %q: %v", c.Val(), err)
				}
				if d <= 0 {
					return nil, c.Errf("refresh %q must be positive", c.Val())
				}
				z.refresh = d
			case "ttl":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid ttl %q: %v", c.Val(), err)
				}
				if secs := d / time.Second; secs > 0 {
					z.ttl = uint32(secs)
				} else {
					return nil, c.Errf("ttl %q must be at least 1s", c.Val())
				}
			case "network":
				args := c.RemainingArgs()
				if len(args) != 2 {
					return nil, c.ArgErr()
				}
				nw, err := newNetwork(args[0], args[1])
				if err != nil {
					return nil, c.Errf("%v", err)
				}
				networks = append(networks, nw)
			case "fallthrough":
				z.fall = true
			case "insecure_skip_verify":
				insecure = true
			default:
				return nil, c.Errf("unknown property %q", c.Val())
			}
		}
	}

	if len(networks) == 0 {
		return nil, c.Errf("must specify at least one zone:networkID pair")
	}
	if token == "" {
		token = os.Getenv("ZTNET_API_TOKEN")
	}
	if token == "" {
		return nil, c.Errf("no ZTNET API token: set token in the Corefile or ZTNET_API_TOKEN")
	}

	z.networks = networks
	z.zones = make([]string, 0, len(networks))
	seen := make(map[string]bool)
	for _, nw := range networks {
		if !seen[nw.zone] {
			seen[nw.zone] = true
			z.zones = append(z.zones, nw.zone)
		}
	}
	z.dotZones = make([]string, len(z.zones))
	for i, zn := range z.zones {
		z.dotZones[i] = "." + zn
	}
	z.api = newAPIClient(api, token, insecure)
	return z, nil
}

// parseNetworkArg splits a "zone:networkID" argument.
func parseNetworkArg(arg string) (network, error) {
	parts := strings.SplitN(arg, ":", 2)
	if len(parts) != 2 {
		return network{}, fmt.Errorf("invalid network %q: want zone:networkID", arg)
	}
	return newNetwork(parts[0], parts[1])
}

func newNetwork(zone, id string) (network, error) {
	zone = strings.TrimSuffix(strings.TrimSpace(zone), ".")
	if !zonePattern.MatchString(zone) {
		return network{}, fmt.Errorf("invalid zone name %q", zone)
	}
	if !networkIDPattern.MatchString(id) {
		return network{}, fmt.Errorf("invalid ZeroTier network ID %q", id)
	}
	return network{zone: strings.ToLower(zone) + ".", networkID: id}, nil
}
