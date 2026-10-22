package chain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessReceiptRead(t *testing.T) {
	const limit = 1 << 20
	for _, test := range []struct {
		name     string
		readable bool
	}{
		{"ordinary", true},
		{"nested", true},
		{"empty", true},
		{"final_symlink", false},
		{"intermediate_symlink", false},
		{"process_directory_symlink", false},
		{"process_directory_ancestor_symlink", false},
		{"fifo", false},
		{"over_limit", false},
		{"at_limit", true},
		{"hardlink", false},
		{"parent_component", false},
		{"cleanable_parent_component", false},
		{"absolute", false},
		{"directory", false},
		{"missing", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Resolve the test runner's temporary-directory aliases (such as
			// /var on Darwin) before creating the deliberately unsafe paths.
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(base, "work")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			receipt := "receipt.txt"
			content := "receipt bytes\x00\xff\r\n日本語\n"
			if test.name == "empty" {
				content = ""
			}
			if test.name == "at_limit" || test.name == "over_limit" {
				content = strings.Repeat("x", limit)
				if test.name == "over_limit" {
					content += "x"
				}
			}
			path := filepath.Join(directory, receipt)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			switch test.name {
			case "nested", "intermediate_symlink", "cleanable_parent_component":
				nested := filepath.Join(directory, "nested")
				if err := os.Mkdir(nested, 0700); err != nil {
					t.Fatal(err)
				}
				if test.name == "cleanable_parent_component" {
					receipt = "nested/../receipt.txt"
					break
				}
				if err := os.Rename(path, filepath.Join(nested, receipt)); err != nil {
					t.Fatal(err)
				}
				receipt = "nested/receipt.txt"
				if test.name == "intermediate_symlink" {
					if err := os.Symlink("nested", filepath.Join(directory, "alias")); err != nil {
						t.Fatal(err)
					}
					receipt = "alias/receipt.txt"
				}
			case "final_symlink", "hardlink":
				receipt = "alias.txt"
				link := os.Symlink
				if test.name == "hardlink" {
					link = os.Link
				}
				if err := link(path, filepath.Join(directory, receipt)); err != nil {
					t.Fatal(err)
				}
			case "process_directory_symlink", "process_directory_ancestor_symlink":
				alias := filepath.Join(base, "alias")
				target := directory
				if test.name == "process_directory_ancestor_symlink" {
					target = base
				}
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				directory = alias
				if test.name == "process_directory_ancestor_symlink" {
					directory = filepath.Join(alias, "work")
				}
			case "fifo", "directory", "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if test.name == "fifo" {
					err = syscall.Mkfifo(path, 0600)
				} else if test.name == "directory" {
					err = os.Mkdir(path, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "parent_component":
				if err := os.Rename(path, filepath.Join(base, receipt)); err != nil {
					t.Fatal(err)
				}
				receipt = "../receipt.txt"
			case "absolute":
				receipt = path
			}
			p := Process{Command: []string{"/bin/sh", "-c", "printf 'process succeeded'"}, Directory: directory, Receipt: receipt}
			result := runReceiptProcess(t, p)
			if test.readable {
				want := receipt + ", read back by the runtime:\n" + content
				if result.Receipt != want {
					t.Fatalf("receipt bytes changed: got %d bytes, want %d", len(result.Receipt), len(want))
				}
			} else if !strings.HasPrefix(result.Receipt, receipt+" could not be read back: ") {
				t.Fatalf("unsafe receipt was not rejected (record has %d bytes)", len(result.Receipt))
			}
		})
	}
}

func TestProcessReceiptRelativeDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	if err := os.Mkdir("work", 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"receipt.txt", "work/receipt.txt"} {
		if err := os.WriteFile(path, []byte("unchanged\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{"", ".", "work", "./work", "work/.."} {
		t.Run(directory, func(t *testing.T) {
			result := runReceiptProcess(t, Process{Command: []string{"/bin/sh", "-c", "printf 'process succeeded'"}, Directory: directory, Receipt: "receipt.txt"})
			if result.Receipt != "receipt.txt, read back by the runtime:\nunchanged\n" {
				t.Fatalf("relative directory receipt changed: %q", result.Receipt)
			}
		})
	}
	if err := os.Symlink("work", "alias"); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"alias", "alias/.."} {
		t.Run(directory, func(t *testing.T) {
			result := runReceiptProcess(t, Process{Command: []string{"/bin/sh", "-c", "printf 'process succeeded'"}, Directory: directory, Receipt: "receipt.txt"})
			if !strings.HasPrefix(result.Receipt, "receipt.txt could not be read back: ") {
				t.Fatal("relative process directory link was followed")
			}
		})
	}
}

func runReceiptProcess(t *testing.T, p Process) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	returned := make(chan Result, 1)
	go func() { returned <- p.run(ctx, Role{Name: "deliver"}, Assignment{}, State{}) }()
	select {
	case result := <-returned:
		if result.Error != "" || result.Interrupted || result.Output != "process succeeded" {
			t.Fatalf("receipt handling changed process success: error=%q interrupted=%v output=%q", result.Error, result.Interrupted, result.Output)
		}
		return result
	case <-ctx.Done():
		t.Fatal("process did not return promptly while reading its receipt")
		return Result{}
	}
}
