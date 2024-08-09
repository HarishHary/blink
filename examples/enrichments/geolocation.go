package enrichments

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/events"
)

// GeoLocationEnrichment enriches the event with geolocation data
type geoLocationEnrichment struct {
	enrichments.Enrichment
}

func (e *geoLocationEnrichment) Enrich(event events.Event) errors.Error {
	if ip, ok := event["IP"].(string); ok {
		geoLocation, err := getGeoLocation(ip)
		if err != nil {
			return errors.New(err)
		}
		event["geoLocation"] = geoLocation
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

var geoLocationEnrichmentInstance, _ = enrichments.NewEnrichment(
	"Geo Location enrichment",
	enrichments.WithDescription("Enrich with geolocation"),
	enrichments.WithEnabled(false),
)
var GeoLocationEnrichment = geoLocationEnrichment{
	Enrichment: *geoLocationEnrichmentInstance,
}
