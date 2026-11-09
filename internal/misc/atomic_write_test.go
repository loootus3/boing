package misc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-x.json")
	if err := os.WriteFile(path, []byte(`{"refresh_token":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte(`{"refresh_token":"new"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != `{"refresh_token":"new"}` {
		t.Fatalf("content = %q", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Fatalf("existing file should keep its mode, got %v", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}

	// New files get perm; a symlink keeps pointing at its target, which gets the new content.
	fresh := filepath.Join(dir, "fresh.json")
	if err := WriteFileAtomic(fresh, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(fresh); st.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %v", st.Mode().Perm())
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(fresh, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(link, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if got, _ := os.ReadFile(fresh); string(got) != "b" {
		t.Fatalf("symlink target content = %q", got)
	}
	_ = os.Remove(link)
	_ = os.Remove(fresh)

	// A failed write (here: the target is a directory, so the rename fails) keeps the old file
	// and removes the temporary one.
	sub := filepath.Join(dir, "sub.json")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(sub, []byte("y"), 0o600); err == nil {
		t.Fatal("expected an error writing over a non-empty directory")
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temporary file left behind after a failure: %v", entries)
	}
}
