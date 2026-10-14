package main

import (
	"bytes"
	"testing"
)

func TestBeadsGuardedMetadataBatchCommandRegistered(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newBeadsCmd(&stdout, &stderr)
	command, _, err := root.Find([]string{"guarded-metadata-batch"})
	if err != nil || command == root || command.Name() != "guarded-metadata-batch" {
		t.Fatalf("guarded-metadata-batch command unavailable: command=%v err=%v", command, err)
	}
	for _, flag := range []string{"store-ref", "snapshot", "request", "json"} {
		if command.Flags().Lookup(flag) == nil {
			t.Errorf("guarded-metadata-batch has no --%s flag", flag)
		}
	}
}
