package orchestrator

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

// TestSelfUpdateRunArgs_ReproducesLiveContainerConfig verifies the docker-run
// argument list rebuilt from a live container's inspect data carries over
// its binds, published ports, network, and env -- everything the helper
// container needs to recreate an equivalent container on the new image.
func TestSelfUpdateRunArgs_ReproducesLiveContainerConfig(t *testing.T) {
	inspect := types.ContainerJSON{
		Config: &container.Config{
			Env: []string{"CLAWORC_DATA_PATH=/app/data", "FOO=bar"},
		},
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				Binds: []string{
					"/var/run/docker.sock:/var/run/docker.sock",
					"/home/user/.claworc/data:/home/user/.claworc/data",
				},
				PortBindings: nat.PortMap{
					"8000/tcp": []nat.PortBinding{{HostIP: "", HostPort: "8000"}},
					"2222/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "2222"}},
				},
			},
		},
	}
	inspect.NetworkSettings = &types.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{
			"claworc": {},
		},
	}

	args := selfUpdateRunArgs("claworc", "claworc/claworc:latest", inspect)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--name claworc",
		"--restart unless-stopped",
		"-v /var/run/docker.sock:/var/run/docker.sock",
		"-v /home/user/.claworc/data:/home/user/.claworc/data",
		"-p 8000:8000",
		"-p 127.0.0.1:2222:2222",
		"--network claworc",
		"-e CLAWORC_DATA_PATH=/app/data",
		"-e FOO=bar",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected run args to contain %q, got: %s", want, joined)
		}
	}
	if args[len(args)-1] != "claworc/claworc:latest" {
		t.Errorf("expected last arg to be the target image, got %q", args[len(args)-1])
	}
}

// TestSelfUpdateHelperScript_WaitsThenRecreates checks the generated helper
// script waits on the named container, force-stops/removes it, and finally
// invokes `docker run` with the exact recreated args -- the sequence the
// disposable updater container depends on.
func TestSelfUpdateHelperScript_WaitsThenRecreates(t *testing.T) {
	script := selfUpdateHelperScript("claworc", []string{"run", "-d", "--name", "claworc", "claworc/claworc:latest"})

	for _, want := range []string{
		"docker inspect -f",
		"docker stop",
		"docker rm -f",
		"docker 'run' '-d' '--name' 'claworc' 'claworc/claworc:latest'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("expected helper script to contain %q, got:\n%s", want, script)
		}
	}

	// The wait/stop/remove steps must precede the recreate step.
	waitIdx := strings.Index(script, "docker inspect")
	stopIdx := strings.Index(script, "docker stop")
	rmIdx := strings.Index(script, "docker rm -f")
	runIdx := strings.LastIndex(script, "docker 'run'")
	if !(waitIdx < stopIdx && stopIdx < rmIdx && rmIdx < runIdx) {
		t.Errorf("expected script steps in order wait < stop < rm < run, got indices %d,%d,%d,%d", waitIdx, stopIdx, rmIdx, runIdx)
	}
}

// TestShellQuote_EscapesEmbeddedSingleQuotes ensures values containing a
// single quote (e.g. an env var value) round-trip safely through the sh -c
// script the helper container runs.
func TestShellQuote_EscapesEmbeddedSingleQuotes(t *testing.T) {
	got := shellQuote(`it's a test`)
	want := `'it'\''s a test'`
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", `it's a test`, got, want)
	}
}
