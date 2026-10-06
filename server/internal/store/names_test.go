package store_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"websitemapper/internal/store"
)

func TestNamesRememberAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	n, err := store.OpenNames(dir, 30*24*time.Hour, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := n.Load("crt.sh", "example.com"); ok {
		t.Error("nothing was saved yet")
	}
	n.Save("crt.sh", "example.com", []string{"a.example.com", "b.example.com"})
	n.Save("crt.sh", "empty.example", nil)
	n.Save("crt.sh", "example.com", []string{"a.example.com", "c.example.com"}) // replaces
	n.Close()

	n, err = store.OpenNames(dir, 30*24*time.Hour, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	hosts, at, ok := n.Load("crt.sh", "example.com")
	if !ok || !reflect.DeepEqual(hosts, []string{"a.example.com", "c.example.com"}) || time.Since(at) > time.Minute {
		t.Errorf("after restart = %v, %v, %v", hosts, at, ok)
	}
	// "The provider knows no names" is an answer too.
	if hosts, _, ok := n.Load("crt.sh", "empty.example"); !ok || len(hosts) != 0 {
		t.Errorf("empty answer = %v, %v", hosts, ok)
	}
	if _, _, ok := n.Load("certspotter", "example.com"); ok {
		t.Error("answers are kept per provider")
	}
}

func TestNamesExpire(t *testing.T) {
	dir := t.TempDir()
	n, _ := store.OpenNames(dir, time.Hour, quiet)
	n.Save("crt.sh", "example.com", []string{"a.example.com"})
	n.Close()
	// Reopened with a shorter limit, after that long has passed.
	time.Sleep(2100 * time.Millisecond)
	n, _ = store.OpenNames(dir, time.Second, quiet)
	defer n.Close()
	if _, _, ok := n.Load("crt.sh", "example.com"); ok {
		t.Error("an answer older than the limit must not be returned")
	}
}

// The scan store treats every .db file in the data directory as a scan and
// removes ones it cannot read; the name memory must not be mistaken for one.
func TestNamesFileIsLeftAloneByTheScanStore(t *testing.T) {
	dir := t.TempDir()
	n, _ := store.OpenNames(dir, time.Hour, quiet)
	n.Save("crt.sh", "example.com", []string{"a.example.com"})
	n.Close()

	s, err := store.Open(store.Options{Dir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, "names.sqlite")); err != nil {
		t.Fatalf("the name memory was removed: %v", err)
	}
	n, _ = store.OpenNames(dir, time.Hour, quiet)
	defer n.Close()
	if _, _, ok := n.Load("crt.sh", "example.com"); !ok {
		t.Error("the remembered answer was lost")
	}
}
