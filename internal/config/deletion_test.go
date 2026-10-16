package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestDeletionFencePreservesBaseCapabilitiesAndRejectsDowngrade(t *testing.T) {
	for _, native := range []bool{false, true} {
		c := Config{MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true, SourceSetPublicationProtection: true, DeletionProtection: true, NativeAuthorityProtection: native}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(`"owned-deletion-v18"`)) {
			t.Fatal("missing deletion fence")
		}
		var read Config
		if err = json.Unmarshal(raw, &read); err != nil || !read.DeletionProtection || !read.SourceSetPublicationProtection || read.SchemaVersion != 17 || read.NativeAuthorityProtection != native {
			t.Fatal(read, err)
		}
		read.DeletionProtection = false
		sticky := read
		PreserveWriterFence(&sticky, c)
		if !sticky.DeletionProtection {
			t.Fatal("rollback dropped deletion floor")
		}
		unwrapped, _, err := unwrapDeletionConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(unwrapped, &read); err == nil {
			t.Fatal("bare deletion flag accepted")
		}
	}
}

func FuzzDeletionConfigurationFenceRoundTrip(f *testing.F) {
	seed, err := json.Marshal(Config{MachineID: "synthetic", RetentionDays: 90, DurableStorageProtection: true, SourceSetPublicationProtection: true, DeletionProtection: true})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"schema_version":1,"machine_id":"synthetic"}`))
	f.Add([]byte(`{"schema_version":{"version":18,"writer":"owned-deletion-v18","base":1},"deletion_protection":true}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var c Config
		if err := json.Unmarshal(raw, &c); err != nil {
			return
		}
		encoded, err := json.Marshal(c)
		if err != nil {
			t.Fatal("accepted configuration cannot be saved", err)
		}
		var restored Config
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal("saved configuration cannot be read", err)
		}
		if restored.DeletionProtection != c.DeletionProtection || restored.SourceSetPublicationProtection != c.SourceSetPublicationProtection || restored.NativeAuthorityProtection != c.NativeAuthorityProtection || restored.SearchLifecycleProtection != c.SearchLifecycleProtection {
			t.Fatal("round trip changed writer capabilities")
		}
	})
}
