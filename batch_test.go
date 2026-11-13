package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchProtectsInputPair(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"same.pdf", "same.ofd"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, failed := runBatch(dir, batchOpts{dpi: 150, quality: 85, force: true}, func(string, ...interface{}) {})
	if failed != 2 {
		t.Fatalf("conflicting inputs: failures=%d", failed)
	}
	for _, name := range []string{"same.pdf", "same.ofd"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != name {
			t.Fatalf("input changed: %s (%v)", name, err)
		}
	}
}

func TestBatchFailurePreservesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.ofd"), []byte("broken ZIP"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(out, "bad.pdf")
	if err := os.WriteFile(target, []byte("keep original"), 0600); err != nil {
		t.Fatal(err)
	}
	_, failed := runBatch(dir, batchOpts{dpi: 150, quality: 85, force: true, outDir: out}, func(string, ...interface{}) {})
	if failed != 1 {
		t.Fatalf("want failure, got %d", failed)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep original" {
		t.Fatalf("existing output changed: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pdf-to-ofd-") {
			t.Fatal("temporary output leaked")
		}
	}
}
