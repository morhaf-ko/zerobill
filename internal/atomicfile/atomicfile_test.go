package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCreateDoesNotReplace(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		var err error
		if symlink {
			err = os.Symlink("missing.yaml", path)
		} else {
			err = os.WriteFile(path, []byte("original"), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := Create(path, []byte("replacement")); !errors.Is(err, os.ErrExist) {
			t.Fatalf("want exists: %v", err)
		}
		if symlink {
			if target, err := os.Readlink(path); err != nil || target != "missing.yaml" {
				t.Fatalf("symlink changed: %s %v", target, err)
			}
		} else if raw, err := os.ReadFile(path); err != nil || string(raw) != "original" {
			t.Fatalf("file changed: %q %v", raw, err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("temporary files left behind: %v", entries)
		}
	}
}

func TestConcurrentCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, raw := range []string{"first", "second"} {
		wg.Go(func() { results <- Create(path, []byte(raw)) })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if successes != 1 || err != nil || (string(raw) != "first" && string(raw) != "second") {
		t.Fatalf("successes %d, content %q: %v", successes, raw, err)
	}
}
