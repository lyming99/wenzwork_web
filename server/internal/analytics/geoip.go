package analytics

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"github.com/oschwald/geoip2-golang/v2"
)

type GeoIPResolver struct {
	reader *geoip2.Reader
}

func NewGeoIPResolver(databasePath string) (*GeoIPResolver, error) {
	databasePath = strings.TrimSpace(databasePath)
	if databasePath == "" {
		return nil, errors.New("GeoIP City database path is required")
	}
	reader, err := geoip2.Open(databasePath)
	if err != nil {
		return nil, err
	}
	return &GeoIPResolver{reader: reader}, nil
}

func (r *GeoIPResolver) Close() error {
	if r == nil || r.reader == nil {
		return nil
	}
	return r.reader.Close()
}

func (r *GeoIPResolver) Lookup(ip netip.Addr) (Location, error) {
	if localAddress(ip) {
		return localNetworkLocation(), nil
	}
	if r == nil || r.reader == nil {
		return Location{}, nil
	}
	record, err := r.reader.City(ip)
	if err != nil {
		return Location{}, err
	}
	if record == nil || !record.HasData() {
		return Location{}, nil
	}
	location := Location{
		CountryCode: record.Country.ISOCode,
		CountryName: localizedName(record.Country.Names),
		CityName:    localizedName(record.City.Names),
		Source:      "maxmind",
	}
	if len(record.Subdivisions) > 0 {
		location.RegionName = localizedName(record.Subdivisions[0].Names)
	}
	return location, nil
}

type NoopLocationResolver struct{}

func (NoopLocationResolver) Lookup(ip netip.Addr) (Location, error) {
	if localAddress(ip) {
		return localNetworkLocation(), nil
	}
	return Location{}, nil
}

func localizedName(names geoip2.Names) string {
	if names.SimplifiedChinese != "" {
		return names.SimplifiedChinese
	}
	return names.English
}

func localAddress(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

func localNetworkLocation() Location {
	return Location{CountryCode: "ZZ", CountryName: "本地网络", RegionName: "本地网络", Source: "local"}
}

type FallbackLocationResolver struct {
	resolvers []LocationResolver
}

func NewFallbackLocationResolver(resolvers ...LocationResolver) LocationResolver {
	filtered := make([]LocationResolver, 0, len(resolvers))
	for _, resolver := range resolvers {
		if resolver != nil {
			filtered = append(filtered, resolver)
		}
	}
	if len(filtered) == 0 {
		return NoopLocationResolver{}
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &FallbackLocationResolver{resolvers: filtered}
}

func (r *FallbackLocationResolver) Lookup(ip netip.Addr) (Location, error) {
	return r.LookupContext(context.Background(), ip)
}

func (r *FallbackLocationResolver) LookupContext(ctx context.Context, ip netip.Addr) (Location, error) {
	var lookupErrors []error
	for _, resolver := range r.resolvers {
		location, err := lookupLocation(ctx, resolver, ip)
		if err != nil {
			lookupErrors = append(lookupErrors, err)
			continue
		}
		if hasLocation(location) {
			return location, nil
		}
	}
	return Location{}, errors.Join(lookupErrors...)
}

func lookupLocation(ctx context.Context, resolver LocationResolver, ip netip.Addr) (Location, error) {
	if resolverWithContext, ok := resolver.(contextLocationResolver); ok {
		return resolverWithContext.LookupContext(ctx, ip)
	}
	return resolver.Lookup(ip)
}

func hasLocation(location Location) bool {
	return strings.TrimSpace(location.CountryCode) != "" ||
		strings.TrimSpace(location.CountryName) != "" ||
		strings.TrimSpace(location.RegionName) != "" ||
		strings.TrimSpace(location.CityName) != ""
}
