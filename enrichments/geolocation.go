package enrichments

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/harishhary/blink/src/shared/enrichments"
)

// GeoLocationEnrichment enriches the event with geolocation data
type geoLocationEnrichment struct {
	enrichments.Enrichment
}

func (e *geoLocationEnrichment) Enrich(ctx context.Context, record map[string]interface{}) error {
	if ip, ok := record["IP"].(string); ok {
		geoLocation, err := getGeoLocation(ip)
		if err != nil {
			return err
		}
		record["geoLocation"] = geoLocation
		return nil
	}
	return nil
}

func getGeoLocation(ip string) (string, error) {
	url := fmt.Sprintf("https://api.ipgeolocation.io/ipgeo?apiKey=your_api_key&ip=%s", ip)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var geoLocation string
	if err := json.NewDecoder(resp.Body).Decode(&geoLocation); err != nil {
		return "", err
	}
	return geoLocation, nil
}

var GeoLocationEnrichment = geoLocationEnrichment{
	Enrichment: enrichments.NewEnrichment(
		"Geo Location enrichment",
		enrichments.Description("Enrich with geolocation"),
		enrichments.Disabled(false),
	),
}
