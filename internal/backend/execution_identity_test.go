package backend

import "testing"

func TestWorkloadIdentitySeparatesRawUnixBytes(t *testing.T) {
	directory := t.TempDir()
	first := identityForTest(t, []string{"/bin/sh", "-c", "exit 0", "arg-\xff"}, directory, map[string]string{"QUEUE_BYTES": "value-\xff"})
	second := identityForTest(t, []string{"/bin/sh", "-c", "exit 0", "arg-\xfe"}, directory, map[string]string{"QUEUE_BYTES": "value-\xff"})
	if first["family"] == second["family"] || first["variant"] == second["variant"] {
		t.Fatal("Distinct raw argument bytes shared workload identities")
	}
	third := identityForTest(t, []string{"/bin/sh", "-c", "exit 0", "arg-\xff"}, directory, map[string]string{"QUEUE_BYTES": "value-\xfe"})
	if first["family"] != third["family"] || first["variant"] == third["variant"] {
		t.Fatal("Distinct raw environment bytes shared workload variants")
	}
	replacement := identityForTest(t, []string{"/bin/sh", "-c", "exit 0", "arg-\ufffd"}, directory, map[string]string{"QUEUE_BYTES": "value-\xff"})
	if first["family"] == replacement["family"] || first["variant"] == replacement["variant"] {
		t.Fatal("Raw bytes collided with valid Unicode replacement character")
	}
	first = identityForTest(t, []string{"/usr/bin/env"}, directory, map[string]string{"NAME-\xff": "value"})
	second = identityForTest(t, []string{"/usr/bin/env"}, directory, map[string]string{"NAME-\xfe": "value"})
	if first["family"] != second["family"] || first["variant"] == second["variant"] {
		t.Fatal("Distinct raw environment names shared workload variants")
	}
}
