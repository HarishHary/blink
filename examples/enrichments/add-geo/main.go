package main

import (
	"context"
	"net"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/enrichments"
)

// addGeo annotates each alert with geo_country and geo_is_internal derived
// from the source_ip field in the alert event. In production, replace the
// stub lookup with a real GeoIP database (e.g. MaxMind GeoLite2).
//
// All static metadata (name, id, enabled, depends_on, etc.) is declared in
// the companion add-geo.yaml sidecar file.
type addGeo struct{ enrichments.BaseEnrichment }

var privateNets = mustParseCIDRs([]string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		out = append(out, network)
	}
	return out
}

func isPrivate(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, network := range privateNets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// Enrich receives the full alerts.Alert struct serialised to JSON (PascalCase
// field names, no struct tags). Return only the new fields to add; the host
// merges them into alert.Event.
func (addGeo) Enrich(_ context.Context, alert map[string]any) (map[string]any, errors.Error) {
	event, _ := alert["Event"].(map[string]any)
	sourceIP, _ := event["source_ip"].(string)

	internal := isPrivate(sourceIP)

	country := "external"
	if internal {
		country = "internal"
	}

	return map[string]any{
		"geo_country":     country,
		"geo_is_internal": internal,
	}, nil
}

func main() {
	enrichments.Serve(addGeo{})
}
