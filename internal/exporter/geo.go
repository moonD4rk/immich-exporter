package exporter

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

// cityKey identifies a city within a country; city names repeat across
// countries, so the pair is the aggregation unit.
type cityKey struct {
	city, country string
}

type latlon struct {
	lat, lon string
}

type geoSnap struct {
	cities, states, countries, geotagged opt
	byCountry                            map[string]float64 // country -> count
	countryCentroids                     map[string]latlon  // centroid of the country's own assets
	byCity                               map[cityKey]float64
	cityCentroids                        map[cityKey]latlon
}

var (
	// geo
	dCities          = desc("places_cities", "Number of distinct cities with assets.")
	dStates          = desc("places_states", "Number of distinct states/provinces with assets.")
	dCountries       = desc("places_countries", "Number of distinct countries with assets.")
	dGeotagged       = desc("assets_geotagged", "Number of geotagged assets (from /map/markers).")
	dAssetsByCountry = desc("assets_by_country", "Geotagged assets per country. lat/lon = centroid of the country's own assets for a coords-mode geomap (no country-name mapping needed; works for every country).", "country", "lat", "lon")
	dAssetsByCity    = desc("assets_by_city", "Geotagged assets per city. lat/lon = centroid of the city's own assets for a coords-mode geomap.", "city", "country", "lat", "lon")
)

func (p *poller) collectGeo(ctx context.Context, s *snapshot) error {
	g := &s.geo
	if cities, err := p.c.Suggest(ctx, "city"); err == nil {
		g.cities = set(float64(len(cities)))
	}
	if states, err := p.c.Suggest(ctx, "state"); err == nil {
		g.states = set(float64(len(states)))
	}
	if countries, err := p.c.Suggest(ctx, "country"); err == nil {
		g.countries = set(float64(len(countries)))
	}
	// One /map/markers call yields the geotagged total, the per-country and
	// per-city splits, and each group's asset centroid (mean lat/lon) for a
	// coords-mode geomap — no place-name → ISO mapping required.
	type marker struct {
		City    string  `json:"city"`
		Country string  `json:"country"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
	}
	type acc struct{ sumLat, sumLon, count float64 }
	var geotagged float64
	byCountry, byCity := map[string]float64{}, map[cityKey]float64{}
	countryAgg, cityAgg := map[string]*acc{}, map[cityKey]*acc{}
	err := immich.Each(ctx, p.c, "/map/markers", func(m marker) {
		geotagged++
		country := strings.TrimSpace(m.Country)
		if country == "" {
			country = "unknown"
		}
		city := strings.TrimSpace(m.City)
		if city == "" {
			city = "unknown"
		}
		ck := cityKey{city: city, country: country}
		byCountry[country]++
		byCity[ck]++
		if country != "unknown" {
			a := countryAgg[country]
			if a == nil {
				a = &acc{}
				countryAgg[country] = a
			}
			a.sumLat, a.sumLon, a.count = a.sumLat+m.Lat, a.sumLon+m.Lon, a.count+1
		}
		if city != "unknown" {
			a := cityAgg[ck]
			if a == nil {
				a = &acc{}
				cityAgg[ck] = a
			}
			a.sumLat, a.sumLon, a.count = a.sumLat+m.Lat, a.sumLon+m.Lon, a.count+1
		}
	})
	if err != nil {
		return err // a listing cut short must not pass for the whole library
	}
	g.geotagged, g.byCountry, g.byCity = set(geotagged), byCountry, byCity
	for country, a := range countryAgg {
		// Round to ~11 km so the centroid label stays stable as assets are added.
		g.countryCentroids[country] = latlon{
			lat: strconv.FormatFloat(a.sumLat/a.count, 'f', 1, 64),
			lon: strconv.FormatFloat(a.sumLon/a.count, 'f', 1, 64),
		}
	}
	for ck, a := range cityAgg {
		// ~1.1 km: city clusters are tight, so finer rounding stays stable while
		// keeping the marker inside the city.
		g.cityCentroids[ck] = latlon{
			lat: strconv.FormatFloat(a.sumLat/a.count, 'f', 2, 64),
			lon: strconv.FormatFloat(a.sumLon/a.count, 'f', 2, 64),
		}
	}
	foldCities(g, p.cfg.CityLimit)
	return nil
}

// foldCities keeps the n cities with the most assets and sums the rest into one
// "other" city without coordinates, bounding immich_assets_by_city for libraries
// that span thousands of places. n <= 0 keeps every city.
func foldCities(g *geoSnap, n int) {
	if n <= 0 || len(g.byCity) <= n {
		return
	}
	keys := slices.Collect(maps.Keys(g.byCity))
	slices.SortFunc(keys, func(a, b cityKey) int {
		return cmp.Or(cmp.Compare(g.byCity[b], g.byCity[a]), cmp.Compare(a.country, b.country), cmp.Compare(a.city, b.city))
	})
	other := cityKey{city: "other"}
	for _, k := range keys[n:] {
		g.byCity[other] += g.byCity[k]
		delete(g.byCity, k)
		delete(g.cityCentroids, k)
	}
}

func emitGeo(ch chan<- prometheus.Metric, s *snapshot) {
	g := &s.geo
	emitOpt(ch, dCities, g.cities)
	emitOpt(ch, dStates, g.states)
	emitOpt(ch, dCountries, g.countries)
	emitOpt(ch, dGeotagged, g.geotagged)
	for k, v := range g.byCountry {
		c := g.countryCentroids[k]
		emit(ch, dAssetsByCountry, v, k, c.lat, c.lon)
	}
	for k, v := range g.byCity {
		c := g.cityCentroids[k]
		emit(ch, dAssetsByCity, v, k.city, k.country, c.lat, c.lon)
	}
}
