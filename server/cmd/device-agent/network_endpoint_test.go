package main

import "testing"

func TestValidateTargetControlURLAcceptsHTTPAndHTTPS(t *testing.T) {
	for input, expected := range map[string]string{
		"http://control.example.test:8080/base/": "http://control.example.test:8080/base",
		"https://control.example.test/":          "https://control.example.test",
	} {
		parsed, err := validateTargetControlURL(input)
		if err != nil || parsed.String() != expected {
			t.Fatalf("validateTargetControlURL(%q) = %v, %v; want %q, nil", input, parsed, err, expected)
		}
	}

	for _, input := range []string{
		"ftp://control.example.test",
		"http://user:password@control.example.test",
		"http://control.example.test?token=secret",
		"http:///missing-host",
	} {
		if _, err := validateTargetControlURL(input); err == nil {
			t.Fatalf("validateTargetControlURL(%q) succeeded", input)
		}
	}
}

func TestValidRelayEndpointAcceptsExplicitWSOrWSS(t *testing.T) {
	for _, endpoint := range []string{
		"ws://127.0.0.1:8443/v1/connect",
		"wss://relay.example.test/v1/connect",
	} {
		if !validRelayEndpoint(endpoint) {
			t.Fatalf("validRelayEndpoint(%q) = false", endpoint)
		}
	}
	for _, endpoint := range []string{
		"https://relay.example.test/v1/connect",
		"wss://relay.example.test/other",
		"wss://user@relay.example.test/v1/connect",
	} {
		if validRelayEndpoint(endpoint) {
			t.Fatalf("validRelayEndpoint(%q) = true", endpoint)
		}
	}
}
