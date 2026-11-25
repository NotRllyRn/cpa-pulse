package config

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"reflect"
	"testing"
)

func TestIgnitionWindows(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       []string
		invalid    bool
	}{
		{name: "omitted", want: []string{"short", "long"}},
		{name: "null", yaml: "ignition:\n  windows: null\n", want: []string{"short", "long"}},
		{name: "empty", yaml: "ignition:\n  windows: []\n", want: []string{}},
		{name: "short", yaml: "ignition:\n  windows: [short]\n", want: []string{"short"}},
		{name: "long", yaml: "ignition:\n  windows: [long]\n", want: []string{"long"}},
		{name: "duplicates", yaml: "ignition:\n  windows: [long, short, long, short]\n", want: []string{"long", "short"}},
		{name: "unknown", yaml: "ignition:\n  windows: [short, monthly]\n", invalid: true},
		{name: "empty-value", yaml: "ignition:\n  windows: ['']\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted unsupported window class")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(cfg.Ignition.Windows, tc.want) {
				t.Fatalf("windows=%#v err=%v", cfg.Ignition.Windows, err)
			}
			for _, class := range []string{"short", "long"} {
				want := false
				for _, enabled := range tc.want {
					want = want || class == enabled
				}
				if cfg.Ignition.WindowEnabled(class) != want {
					t.Fatalf("enabled(%s)", class)
				}
			}
		})
	}
}

func TestEmptyIgnitionWindowsRoundTrip(t *testing.T) {
	cfg, err := Parse([]byte("ignition:\n  windows: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			var data []byte
			var err error
			if format == "yaml" {
				data, err = yaml.Marshal(cfg)
			} else {
				data, err = json.Marshal(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Parse(data)
			if err != nil || roundTrip.Ignition.Windows == nil || len(roundTrip.Ignition.Windows) != 0 {
				t.Fatalf("windows=%#v err=%v", roundTrip.Ignition.Windows, err)
			}
			if format == "json" {
				decoded := Default()
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.Ignition.Windows == nil || len(decoded.Ignition.Windows) != 0 {
					t.Fatal("JSON restored defaults")
				}
			}
		})
	}
	if got := Default().Ignition.Windows; !reflect.DeepEqual(got, []string{"short", "long"}) {
		t.Fatalf("defaults=%v", got)
	}
}
