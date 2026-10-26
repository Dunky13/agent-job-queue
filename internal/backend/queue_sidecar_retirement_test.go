package backend

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestQueuePrivateFilesDuringJournalRetirement(t *testing.T) {
	_, store := edgeQueueStore(t)
	done := make(chan error, 1)
	go func() {
		for range 1000 {
			if _, err := store.db.Exec("UPDATE queue_settings SET dirty=1-dirty WHERE id=1"); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var observed error
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if observed != nil {
				t.Fatal(observed)
			}
			return
		default:
			if err := queuePrivateFiles(store.path); err != nil && observed == nil {
				observed = err
			}
			runtime.Gosched()
		}
	}
}

func TestQueuePrivateFilesRechecksOnlyRetiredOwnedSidecars(t *testing.T) {
	private := t.TempDir()
	metadata := func(name string, mode os.FileMode, unlink, hardlink bool) os.FileInfo {
		t.Helper()
		filename := filepath.Join(private, name)
		file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := file.Chmod(mode); err != nil {
			t.Fatal(err)
		}
		if hardlink {
			if err := os.Link(filename, filename+".link"); err != nil {
				t.Fatal(err)
			}
		}
		if unlink {
			if err := os.Remove(filename); err != nil {
				t.Fatal(err)
			}
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	safe := metadata("safe", 0600, false, false)
	retired := metadata("retired", 0600, true, false)
	public := metadata("public", 0644, false, false)
	retiredPublic := metadata("retired-public", 0644, true, false)
	hardlink := metadata("hardlink", 0600, false, true)
	for _, test := range []struct {
		name      string
		primary   bool
		first     os.FileInfo
		next      os.FileInfo
		wantSafe  bool
		wantCalls int
	}{
		{name: "removed", first: retired, wantSafe: true, wantCalls: 2},
		{name: "safe-replacement", first: retired, next: safe, wantSafe: true, wantCalls: 2},
		{name: "unsafe-replacement", first: retired, next: public, wantCalls: 2},
		{name: "repeated-retirement", first: retired, next: retired, wantCalls: 3},
		{name: "primary-retired", primary: true, first: retired, wantCalls: 1},
		{name: "public-retired", first: retiredPublic, wantCalls: 1},
		{name: "hardlink", first: hardlink, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(private, "queue.sqlite3")
			target := path + "-journal"
			if test.primary {
				target = path
			}
			calls := 0
			err := queuePrivateFilesInspect(path, func(filename string) (os.FileInfo, error) {
				if filename == target {
					calls++
					if calls == 1 {
						return test.first, nil
					}
					if test.next != nil {
						return test.next, nil
					}
				}
				if filename == path {
					return safe, nil
				}
				return nil, os.ErrNotExist
			})
			if (err == nil) != test.wantSafe || calls != test.wantCalls {
				t.Fatalf("safe=%v calls=%d; want safe=%v calls=%d; error=%v", err == nil, calls, test.wantSafe, test.wantCalls, err)
			}
		})
	}
}
