package orchestrator

import "testing"

// TestMapDockerStatusNoHealthcheck pins the behaviour a missing healthcheck
// must have. Docker leaves State.Health nil (so health == "") for a container
// with no healthcheck of its own and no Probes.Liveness, which used to be
// mapped to "creating" -- a status that could never resolve, because nothing
// would ever set a health status later. That left the managed OpenBao
// workload reading "creating" in the UI indefinitely while it was in fact
// serving requests, and kept it permanently ineligible for env propagation
// (which gates on "running").
func TestMapDockerStatusNoHealthcheck(t *testing.T) {
	if got := mapDockerStatus("running", ""); got != "running" {
		t.Errorf("running container with no healthcheck: got %q, want %q", got, "running")
	}
}

func TestMapDockerStatus(t *testing.T) {
	cases := []struct {
		name   string
		status string
		health string
		want   string
	}{
		{"healthy", "running", "healthy", "running"},
		{"unhealthy is an error", "running", "unhealthy", "error"},
		{"still in start period", "running", "starting", "creating"},
		{"no healthcheck configured", "running", "", "running"},
		{"created", "created", "", "creating"},
		{"restarting", "restarting", "", "creating"},
		{"exited", "exited", "", "stopped"},
		{"dead", "dead", "", "stopped"},
		{"paused", "paused", "", "stopped"},
		{"removing", "removing", "", "stopped"},
		{"unknown state", "something-new", "", "stopped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapDockerStatus(tc.status, tc.health); got != tc.want {
				t.Errorf("mapDockerStatus(%q, %q) = %q, want %q", tc.status, tc.health, got, tc.want)
			}
		})
	}
}
