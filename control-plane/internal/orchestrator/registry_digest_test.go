package orchestrator

import "testing"

// TestParseImageRef covers the reference shapes SelfUpdate actually sees in
// practice: bare Docker Hub images/tags, namespaced Docker Hub repos, and
// third-party registries with and without an explicit port, matching the
// same defaulting rules the Docker CLI/daemon apply (missing tag ->
// "latest", missing registry -> Docker Hub, missing namespace on Docker Hub
// -> "library/").
func TestParseImageRef(t *testing.T) {
	cases := []struct {
		ref  string
		want imageRef
	}{
		{
			ref:  "ubuntu",
			want: imageRef{registry: dockerHubHost, repository: "library/ubuntu", tag: "latest"},
		},
		{
			ref:  "ubuntu:22.04",
			want: imageRef{registry: dockerHubHost, repository: "library/ubuntu", tag: "22.04"},
		},
		{
			ref:  "claworc/claworc",
			want: imageRef{registry: dockerHubHost, repository: "claworc/claworc", tag: "latest"},
		},
		{
			ref:  "claworc/claworc:latest",
			want: imageRef{registry: dockerHubHost, repository: "claworc/claworc", tag: "latest"},
		},
		{
			ref:  "ghcr.io/gluk-w/claworc:v1.2.3",
			want: imageRef{registry: "ghcr.io", repository: "gluk-w/claworc", tag: "v1.2.3"},
		},
		{
			ref:  "localhost:5000/myimage:dev",
			want: imageRef{registry: "localhost:5000", repository: "myimage", tag: "dev"},
		},
		{
			ref:  "registry.example.com:443/team/app",
			want: imageRef{registry: "registry.example.com:443", repository: "team/app", tag: "latest"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			got := parseImageRef(tc.ref)
			if got != tc.want {
				t.Errorf("parseImageRef(%q) = %+v, want %+v", tc.ref, got, tc.want)
			}
		})
	}
}

// TestParseBearerChallenge checks the WWW-Authenticate challenge parser
// against the exact header shape Docker Hub and GHCR send.
func TestParseBearerChallenge(t *testing.T) {
	challenge := `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:claworc/claworc:pull"`
	params, err := parseBearerChallenge(challenge)
	if err != nil {
		t.Fatalf("parseBearerChallenge: %v", err)
	}
	if params["realm"] != "https://auth.docker.io/token" {
		t.Errorf("realm = %q", params["realm"])
	}
	if params["service"] != "registry.docker.io" {
		t.Errorf("service = %q", params["service"])
	}
	if params["scope"] != "repository:claworc/claworc:pull" {
		t.Errorf("scope = %q", params["scope"])
	}
}

func TestParseBearerChallenge_RejectsNonBearer(t *testing.T) {
	if _, err := parseBearerChallenge(`Basic realm="x"`); err == nil {
		t.Error("expected error for non-Bearer challenge, got nil")
	}
}
