package main

import (
	"context"
	"net"

	"github.com/harishhary/blink/internal/errors"
	sdk "github.com/harishhary/blink/pkg/tuning_rules/sdk"
)

// boostExternalIP raises alert confidence when the source_ip is not in
// RFC 1918 address space — external origin is a stronger signal.
//
// All static metadata (name, id, enabled, global, rule_type, confidence, etc.)
// is declared in the companion boost-external-ip.yaml sidecar file.
type boostExternalIP struct{ sdk.BaseTuningRule }

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

// Tune returns true when the rule applies — i.e. the source IP is external.
// alert is the full alerts.Alert struct serialised to JSON (no struct tags,
// so field names are PascalCase). The event fields live under "Event".
func (boostExternalIP) Tune(_ context.Context, alert map[string]any) (bool, errors.Error) {
	event, _ := alert["Event"].(map[string]any)
	sourceIP, _ := event["source_ip"].(string)
	return !isPrivate(sourceIP), nil
}

func main() {
	sdk.Serve(boostExternalIP{})
}
