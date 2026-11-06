package app

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
)

func TestSaveStateMergesIndependentChanges(t *testing.T) {
	isolateStateFile(t)
	initial := &AppManager{Apps: map[string]*AppInfo{"first": {ID: "first"}, "second": {ID: "second"}}}
	if err := initial.SaveState(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["first"]["future_field"] = "keep"
	data, _ = json.Marshal(document)
	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		t.Fatal(err)
	}
	left, right := &AppManager{}, &AppManager{}
	for _, manager := range []*AppManager{left, right} {
		if err := manager.LoadState(); err != nil {
			t.Fatal(err)
		}
	}
	left.Apps["first"].Port = 4567
	right.Apps["first"].Watching = true
	delete(left.Apps, "second")
	right.Apps["new"] = &AppInfo{ID: "new", Port: 6789}
	var wg sync.WaitGroup
	for _, manager := range []*AppManager{left, right} {
		wg.Go(func() {
			if err := manager.SaveState(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	after := &AppManager{}
	if err := after.LoadState(); err != nil {
		t.Fatal(err)
	}
	if after.Apps["first"].Port != 4567 || !after.Apps["first"].Watching || after.Apps["new"] == nil || after.Apps["second"] != nil {
		t.Fatal("concurrent saves lost a change or resurrected a deleted app")
	}
	data, _ = os.ReadFile(stateFile)
	if err := json.Unmarshal(data, &document); err != nil || document["first"]["future_field"] != "keep" {
		t.Fatal("save lost an unknown field", err)
	}
}

func TestSaveStateRefusesDamagedFile(t *testing.T) {
	isolateStateFile(t)
	manager := &AppManager{Apps: map[string]*AppInfo{"first": {ID: "first"}}}
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	manager.Apps["first"].Port = 4567
	if err := manager.SaveState(); err == nil {
		t.Fatal("save overwrote damaged state")
	}
	data, _ := os.ReadFile(stateFile)
	if string(data) != "broken" {
		t.Fatal("save changed damaged state")
	}
}
