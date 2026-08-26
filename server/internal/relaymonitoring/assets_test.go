package relaymonitoring

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRelayMonitoringAssetsAreStructuredAndBoundedCardinality(t *testing.T) {
	root := repositoryRoot(t)
	rulesBytes, err := os.ReadFile(filepath.Join(root, "deploy", "monitoring", "relay-alerts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(rulesBytes, &rules); err != nil {
		t.Fatalf("decode Relay alert rules: %v", err)
	}
	if len(rules.Groups) != 1 || len(rules.Groups[0].Rules) < 10 {
		t.Fatalf("Relay alert groups/rules = %d/%d", len(rules.Groups), len(rules.Groups[0].Rules))
	}
	seen := make(map[string]struct{})
	for _, rule := range rules.Groups[0].Rules {
		if strings.TrimSpace(rule.Alert) == "" || strings.TrimSpace(rule.Expr) == "" {
			t.Fatalf("incomplete Relay alert rule: %+v", rule)
		}
		if _, duplicate := seen[rule.Alert]; duplicate {
			t.Fatalf("duplicate Relay alert %q", rule.Alert)
		}
		seen[rule.Alert] = struct{}{}
		for _, forbidden := range []string{"device_id", "deviceId", "user_id", "userId", "installation_id", "instance_id"} {
			if strings.Contains(rule.Expr, forbidden) {
				t.Fatalf("Relay alert %q uses high-cardinality label %q", rule.Alert, forbidden)
			}
		}
	}

	dashboardBytes, err := os.ReadFile(filepath.Join(root, "deploy", "monitoring", "relay-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Title  string `json:"title"`
		Panels []struct {
			ID      int    `json:"id"`
			Title   string `json:"title"`
			Targets []struct {
				Expression string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(dashboardBytes, &dashboard); err != nil {
		t.Fatalf("decode Relay dashboard: %v", err)
	}
	if dashboard.Title != "WenzWork Relay" || len(dashboard.Panels) < 8 {
		t.Fatalf("Relay dashboard title/panels = %q/%d", dashboard.Title, len(dashboard.Panels))
	}
	for _, panel := range dashboard.Panels {
		if panel.ID < 1 || strings.TrimSpace(panel.Title) == "" || len(panel.Targets) == 0 {
			t.Fatalf("incomplete Relay dashboard panel: %+v", panel)
		}
		for _, target := range panel.Targets {
			if !strings.Contains(target.Expression, "wenzwork_relay_") {
				t.Fatalf("Relay dashboard target is not scoped: %q", target.Expression)
			}
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve Relay monitoring test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
