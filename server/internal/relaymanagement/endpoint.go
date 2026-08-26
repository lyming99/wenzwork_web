package relaymanagement

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

func validOptionalPublicEndpoint(value string) bool {
	if value == "" || len(value) > 255 || strings.ContainsAny(value, "\r\n\x00") {
		return value == ""
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss") && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "/v2/connect" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validListenerPort(port int) bool {
	return port >= 1 && port <= 65_535
}

func listenerAddress(port int) string {
	return net.JoinHostPort("", strconv.Itoa(port))
}
