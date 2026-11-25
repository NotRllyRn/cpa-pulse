package store

import (
	"path/filepath"
	"testing"
)

func TestIgnitionWindowLazyMigrationAcrossRestart(t *testing.T) {
	reset := float64(1800000000)
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "existing-wins"}[existing], func(t *testing.T) {
			file := StateFile{Path: filepath.Join(t.TempDir(), "state.json")}
			state := NewState()
			ts := state.Target("account:group")
			ts.RollingReset = true
			ts.ResetObservation = &Observation{SeenEpoch: reset - 60, ResetEpoch: &reset}
			ts.LastSuccessEpoch, ts.LastAttemptEpoch = reset-100, reset-90
			ts.LastModel, ts.ConsecutiveFailures, ts.RetryAtEpoch = "model", 2, reset+300
			if existing {
				ts.Windows = map[string]*IgnitionWindowState{"real-5h": {LastSuccessEpoch: 42}}
			}
			if err := file.Save(state); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := file.Load()
			if err != nil {
				t.Fatal(err)
			}
			ts = loaded.Target("account:group")
			for _, id := range []string{"actual-week", "window-30d"} {
				ws := ts.IgnitionWindow(id, false)
				if *ws != (IgnitionWindowState{}) {
					t.Fatalf("long inherited legacy: %+v", ws)
				}
			}
			if !ts.RollingReset || ts.ResetObservation == nil {
				t.Fatal("long windows consumed legacy migration")
			}
			five := ts.IgnitionWindow("real-5h", true)
			if existing {
				if five.LastSuccessEpoch != 42 || five.RollingReset || five.ResetObservation != nil {
					t.Fatalf("existing overwritten: %+v", five)
				}
			} else if !five.RollingReset || five.ResetObservation == nil || five.LastSuccessEpoch != int64(reset-100) || five.LastAttemptEpoch != int64(reset-90) {
				t.Fatalf("migration: %+v", five)
			}
			if five.PulsedResetEpoch != 0 || ts.RollingReset || ts.ResetObservation != nil || ts.LastModel != "model" || ts.ConsecutiveFailures != 2 || ts.RetryAtEpoch != reset+300 || ts.LastSuccessEpoch != reset-100 {
				t.Fatalf("shared state changed: %+v", ts)
			}
			if err := file.Save(loaded); err != nil {
				t.Fatal(err)
			}
			again, _, err := file.Load()
			if err != nil {
				t.Fatal(err)
			}
			ws := again.Target("account:group").IgnitionWindow("real-5h", true)
			if ws.LastSuccessEpoch != five.LastSuccessEpoch || ws.RollingReset != five.RollingReset || ws.PulsedResetEpoch != 0 {
				t.Fatalf("restart changed migrated state: %+v", ws)
			}
		})
	}
}
