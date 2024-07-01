package enrichments

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/harishhary/blink/src/enrichments"
	"github.com/harishhary/blink/src/events"
)

// GeoLocationEnrichment enriches the event with geolocation data
type GeoLocationEnrichment struct {
	timing enrichments.EnrichmentTiming
}

func (e *GeoLocationEnrichment) Name() string {
	return "Geo Location Enrichment"
}

func (e *GeoLocationEnrichment) Enrich(ctx context.Context, event *events.Event) error {
	geoLocation, err := getGeoLocation(event.IP)
	if err != nil {
		return err
	}
	event.GeoLocation = geoLocation
	return nil
}

func (e *GeoLocationEnrichment) Timing() enrichments.EnrichmentTiming {
	return e.timing
}

func getGeoLocation(ip string) (events.GeoLocation, error) {
	url := fmt.Sprintf("https://api.ipgeolocation.io/ipgeo?apiKey=your_api_key&ip=%s", ip)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return events.GeoLocation{}, err
	}
	defer resp.Body.Close()

	var geoLocation events.GeoLocation
	if err := json.NewDecoder(resp.Body).Decode(&geoLocation); err != nil {
		return events.GeoLocation{}, err
	}
	return geoLocation, nil
}
