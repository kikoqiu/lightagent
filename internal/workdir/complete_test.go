package workdir

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// seedTree builds the one-level tree the completion cases walk: two names that
// share a prefix, one lone name, a directory holding a child, and a hidden file.
func seedTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"alpha.txt", "alphabet.md", "beta.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// candidateNames flattens a listing to its names, so an assertion reads as the
// column the front-ends show.
func candidateNames(candidates []Candidate) []string {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.Name)
	}
	return names
}

// TestCompleteListsOneLevel pins the listing behind the '@' picker: the fragment
// is split at its last separator, the children of that directory come back with
// directories first, and the head is returned exactly as spelled so the caller
// can put it back in front of the picked name.
func TestCompleteListsOneLevel(t *testing.T) {
	dir := seedTree(t)
	sep := string(os.PathSeparator)

	head, got := Complete(dir + sep)
	if head != dir+sep {
		t.Fatalf("head = %q, want %q", head, dir+sep)
	}
	want := []string{"sub", "alpha.txt", "alphabet.md", "beta.txt"}
	if names := candidateNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("Complete(%q) = %v, want %v", dir+sep, names, want)
	}
	if !got[0].Dir {
		t.Errorf("the directory is not marked: %+v", got[0])
	}
	if got[1].Dir {
		t.Errorf("a file is marked as a directory: %+v", got[1])
	}
}

// TestCompleteFiltersAndStaysOneLevel pins the base filter: only the names that
// start with the fragment's last element are offered, the offset lives under an
// absolute head, and a second level needs the separator the user types (only one
// level is ever listed).
func TestCompleteFiltersAndStaysOneLevel(t *testing.T) {
	dir := seedTree(t)
	sep := string(os.PathSeparator)

	head, got := Complete(dir + sep + "alp")
	if head != dir+sep {
		t.Fatalf("head = %q, want %q", head, dir+sep)
	}
	want := []string{"alpha.txt", "alphabet.md"}
	if names := candidateNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("Complete(...alp) = %v, want %v", names, want)
	}

	// The nested level only opens once the separator is there: the fragment
	// "sub" completes the directory's name, it does not look inside it.
	if _, got := Complete(dir + sep + "sub"); len(got) != 1 || got[0].Name != "sub" || !got[0].Dir {
		t.Fatalf("Complete(...sub) = %+v, want just the directory", got)
	}
	head, got = Complete(dir + sep + "sub" + sep + "in")
	if head != dir+sep+"sub"+sep {
		t.Fatalf("head = %q, want %q", head, dir+sep+"sub"+sep)
	}
	if names := candidateNames(got); !reflect.DeepEqual(names, []string{"inner.txt"}) {
		t.Fatalf("Complete(...sub%sin) = %v, want the nested file", sep, names)
	}

	// A directory that cannot be listed has no candidates rather than an error.
	if head, got := Complete(dir + sep + "nope" + sep + "x"); head != dir+sep+"nope"+sep || len(got) != 0 {
		t.Fatalf("Complete on a missing directory = (%q, %v), want the head and no candidates", head, got)
	}
}

// TestCompleteHidesHiddenNamesUntilAsked pins the dot rule a shell uses: a hidden
// entry is offered once the fragment already starts with a dot.
func TestCompleteHidesHiddenNamesUntilAsked(t *testing.T) {
	dir := seedTree(t)
	sep := string(os.PathSeparator)

	if _, got := Complete(dir + sep); len(candidateNames(got)) != 4 {
		t.Fatalf("Complete(...) = %v, want the hidden entry left out", candidateNames(got))
	}
	if _, got := Complete(dir + sep + "."); !reflect.DeepEqual(candidateNames(got), []string{".hidden"}) {
		t.Fatalf("Complete(...%s.) = %v, want the hidden entry", sep, candidateNames(got))
	}
}

// TestCompleteDefaultsToTheWorkingDirectory pins the relative case: an empty head
// is the process working directory, so a bare "@name" completes where the agent
// runs.
func TestCompleteDefaultsToTheWorkingDirectory(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	dir := seedTree(t)
	if _, err := Set(dir); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if head, got := Complete(""); head != "" || len(got) != 4 {
		t.Fatalf("Complete(\"\") = (%q, %v), want the working directory's entries", head, candidateNames(got))
	}
	head, got := Complete("be")
	if head != "" || !reflect.DeepEqual(candidateNames(got), []string{"beta.txt"}) {
		t.Fatalf("Complete(be) = (%q, %v), want the lone file", head, candidateNames(got))
	}
}
