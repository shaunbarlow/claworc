package connectorprov

import (
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
)

func TestBuildSpec_FixedNameAndPort(t *testing.T) {
	spec := buildSpec(Config{
		Image:         "ghcr.io/shaunbarlow/open-connector:tip",
		EncryptionKey: "enc-key",
		AdminToken:    "admin-token",
		StorageSize:   "5Gi",
	})

	if spec.Name != WorkloadName {
		t.Errorf("spec.Name = %q, want %q", spec.Name, WorkloadName)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].ContainerPort != containerPort {
		t.Fatalf("expected a single port %d, got %+v", containerPort, spec.Ports)
	}
	if !spec.Ports[0].Publish {
		t.Errorf("connector HTTP port should be Publish=true so the Docker backend binds a host loopback port")
	}
	if spec.Env["OOMOL_CONNECT_ENCRYPTION_KEY"] != "enc-key" {
		t.Errorf("encryption key env var missing/wrong: %+v", spec.Env)
	}
	if spec.Env["OOMOL_CONNECT_ADMIN_TOKEN"] != "admin-token" {
		t.Errorf("admin token env var missing/wrong: %+v", spec.Env)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Size != "5Gi" || spec.Volumes[0].MountPath != "/app/data" {
		t.Fatalf("expected one data volume sized 5Gi at /app/data, got %+v", spec.Volumes)
	}
	if spec.Pull != orchestrator.PullAlways {
		t.Errorf("spec.Pull = %v, want PullAlways so a mutable tag (e.g. :tip) is re-pulled on every Apply", spec.Pull)
	}
}

func TestBuildSpec_DefaultsStorageWhenEmpty(t *testing.T) {
	spec := buildSpec(Config{Image: "img"})
	if len(spec.Volumes) != 1 || spec.Volumes[0].Size != defaultConnectorStorageForTest {
		t.Fatalf("expected default storage size %q, got %+v", defaultConnectorStorageForTest, spec.Volumes)
	}
}

// defaultConnectorStorageForTest mirrors buildSpec's fallback so the test
// doesn't hardcode the literal twice.
const defaultConnectorStorageForTest = "10Gi"

func TestGenerateEncryptionKey_ReturnsDistinctValues(t *testing.T) {
	a, err := GenerateEncryptionKey()
	if err != nil {
		t.Fatalf("GenerateEncryptionKey: %v", err)
	}
	b, err := GenerateEncryptionKey()
	if err != nil {
		t.Fatalf("GenerateEncryptionKey: %v", err)
	}
	if a == b {
		t.Errorf("expected two distinct generated keys, got the same value twice")
	}
	if len(a) == 0 {
		t.Errorf("expected a non-empty generated key")
	}
}

func TestGenerateAdminToken_ReturnsDistinctValues(t *testing.T) {
	a, err := GenerateAdminToken()
	if err != nil {
		t.Fatalf("GenerateAdminToken: %v", err)
	}
	b, err := GenerateAdminToken()
	if err != nil {
		t.Fatalf("GenerateAdminToken: %v", err)
	}
	if a == b {
		t.Errorf("expected two distinct generated tokens, got the same value twice")
	}
}
