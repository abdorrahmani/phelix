package env

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMasterKeyConcurrentCreation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	keys := make([][]byte, 20)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := GetMasterKey()
			if err != nil {
				t.Error(err)
			}
			keys[i] = key
		}()
	}
	wg.Wait()
	for _, key := range keys {
		if len(key) != 32 || !bytes.Equal(key, keys[0]) {
			t.Fatal("writers received different master keys")
		}
	}
}

func TestMasterKeyCorruptionIsNotOverwritten(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := GetMasterKey(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("HOME"), ".phelix", "master.key")
	for _, data := range []string{"broken", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := GetMasterKey(); err == nil {
			t.Fatal("damaged key was accepted")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatal("damaged key was overwritten")
		}
	}
}
