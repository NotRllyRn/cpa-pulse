package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefusedModelsPrunedOnLoadAndSave(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	file := StateFile{Path: filepath.Join(t.TempDir(), "state.json"), Now: func() time.Time { return now }}
	state := NewState()
	state.RefusedModels = map[string]time.Time{"old": now.Add(-25 * time.Hour), "expired": now.Add(-24 * time.Hour), "recent": now.Add(-23 * time.Hour)}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.RefusedModels) != 1 || loaded.RefusedModels["recent"].IsZero() {
		t.Fatalf("load refusals=%v", loaded.RefusedModels)
	}
	loaded.RefusedModels["old"] = now.Add(-48 * time.Hour)
	if err := file.Save(loaded); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(file.Path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted State
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.RefusedModels) != 1 || len(loaded.RefusedModels) != 1 {
		t.Fatalf("save refusals=%v memory=%v", persisted.RefusedModels, loaded.RefusedModels)
	}
	now = now.Add(2 * time.Hour)
	if err := file.Save(loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.RefusedModels) != 0 {
		t.Fatal("refusals did not expire")
	}
}
