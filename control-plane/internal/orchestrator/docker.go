package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/docker/go-units"
	"github.com/gluk-w/claworc/control-plane/internal/config"
	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

const (
	labelManagedBy = "claworc"
	networkName    = "claworc"
)

var volumeSuffixes = []string{"homebrew", "home"}

type DockerOrchestrator struct {
	client          *dockerclient.Client
	available       bool
	InstanceFactory sshproxy.InstanceFactory
}

func (d *DockerOrchestrator) Initialize(ctx context.Context) error {
	var opts []dockerclient.Opt
	opts = append(opts, dockerclient.FromEnv)
	opts = append(opts, dockerclient.WithAPIVersionNegotiation())
	if config.Cfg.DockerHost != "" {
		opts = append(opts, dockerclient.WithHost(config.Cfg.DockerHost))
	}

	var err error
	d.client, err = dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}

	_, err = d.client.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}

	if err := d.ensureNetwork(ctx); err != nil {
		return fmt.Errorf("docker network: %w", err)
	}

	d.available = true
	log.Println("Docker daemon connected")
	return nil
}

func (d *DockerOrchestrator) ensureNetwork(ctx context.Context) error {
	_, err := d.client.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err == nil {
		return nil
	}
	_, err = d.client.NetworkCreate(ctx, networkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"managed-by": labelManagedBy},
	})
	if err != nil {
		return fmt.Errorf("create network %s: %w", networkName, err)
	}
	log.Printf("Created Docker network: %s", networkName)
	return nil
}

func (d *DockerOrchestrator) IsAvailable(_ context.Context) bool {
	return d.available
}

func (d *DockerOrchestrator) BackendName() string {
	return "docker"
}

// VolumeNameFor returns the canonical Docker volume name for a workload's
// per-suffix data volume (homebrew, home, browser-data, ...). The convention
// is namespaced with the claworc- prefix so volumes are easy to spot when
// inspecting Docker state by hand.
func (d *DockerOrchestrator) VolumeNameFor(name, suffix string) string {
	return fmt.Sprintf("claworc-%s-%s", name, suffix)
}

func (d *DockerOrchestrator) volumeName(name, suffix string) string {
	return d.VolumeNameFor(name, suffix)
}

// CloneVolume copies srcVolName into dstVolName via a one-shot alpine helper.
// The destination volume is created if it does not already exist.
func (d *DockerOrchestrator) CloneVolume(ctx context.Context, srcVolName, dstVolName string) error {
	if _, err := d.client.VolumeInspect(ctx, srcVolName); err != nil {
		// Source volume doesn't exist — nothing to clone. Treat as success so
		// callers can call this unconditionally on every clone.
		return nil
	}
	if _, err := d.client.VolumeCreate(ctx, volume.CreateOptions{
		Name:   dstVolName,
		Labels: map[string]string{"managed-by": labelManagedBy},
	}); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("create dst volume %s: %w", dstVolName, err)
	}
	return d.copyVolume(ctx, srcVolName, dstVolName)
}

// ensureHostBindDir creates a host bind-mount source directory (recursively) if
// it is within the CLAWORC_ALLOWED_HOST_MOUNTS allowlist. Re-checking the
// allowlist here keeps directory creation safe even though the path was already
// validated when the shared folder was created.
func ensureHostBindDir(hostPath string) error {
	clean := filepath.Clean(hostPath)
	allowed := false
	for _, prefix := range config.Cfg.AllowedHostMounts {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		prefix = filepath.Clean(prefix)
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("path is not within an allowed mount prefix")
	}
	if info, err := os.Stat(clean); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("path exists but is not a directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(clean, 0o755)
}

func parseCPUToNanoCPUs(cpuStr string) int64 {
	if strings.HasSuffix(cpuStr, "m") {
		val := cpuStr[:len(cpuStr)-1]
		var n int64
		fmt.Sscanf(val, "%d", &n)
		return n * 1_000_000
	}
	var f float64
	fmt.Sscanf(cpuStr, "%f", &f)
	return int64(f * 1_000_000_000)
}

func parseMemoryToBytes(memStr string) int64 {
	unitMap := map[string]int64{
		"Ki": 1024,
		"Mi": 1024 * 1024,
		"Gi": 1024 * 1024 * 1024,
		"Ti": 1024 * 1024 * 1024 * 1024,
		"K":  1000,
		"M":  1000 * 1000,
		"G":  1000 * 1000 * 1000,
		"T":  1000 * 1000 * 1000 * 1000,
	}
	for suffix, multiplier := range unitMap {
		if strings.HasSuffix(memStr, suffix) {
			val := memStr[:len(memStr)-len(suffix)]
			var n int64
			fmt.Sscanf(val, "%d", &n)
			return n * multiplier
		}
	}
	var n int64
	fmt.Sscanf(memStr, "%d", &n)
	return n
}

func (d *DockerOrchestrator) ensureImage(ctx context.Context, img string) error {
	// Check if image exists locally first
	_, _, err := d.client.ImageInspectWithRaw(ctx, img)
	if err == nil {
		log.Printf("Image %s found locally", utils.SanitizeForLog(img))
		return nil
	}

	// Image not found locally, try to pull
	log.Printf("Image %s not found locally, pulling...", utils.SanitizeForLog(img))
	reader, err := d.client.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer reader.Close()
	io.Copy(io.Discard, reader)
	log.Printf("Image %s pulled successfully", utils.SanitizeForLog(img))
	return nil
}

// CreateInstance ignores params.Ports and params.ServiceAccountAnnotations:
// Docker containers already publish their fixed port set below, and there is
// no ServiceAccount concept outside Kubernetes. Both are K8s-only knobs - see
// KubernetesOrchestrator.CreateInstance.
func (d *DockerOrchestrator) CreateInstance(ctx context.Context, params CreateParams) error {
	progress := params.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	progress("Pulling image...")
	if err := d.ensureImage(ctx, params.ContainerImage); err != nil {
		return err
	}

	// Create volumes
	progress("Creating volumes...")
	for _, suffix := range volumeSuffixes {
		volName := d.volumeName(params.Name, suffix)
		_, err := d.client.VolumeCreate(ctx, volume.CreateOptions{
			Name:   volName,
			Labels: map[string]string{"managed-by": labelManagedBy, "instance": params.Name},
		})
		if err != nil {
			log.Printf("Volume %s may already exist: %v", utils.SanitizeForLog(volName), err)
		}
	}

	// Create shared folder volumes
	d.ensureSharedVolumes(ctx, params.SharedFolderMounts)

	progress("Creating container...")
	return d.createContainer(ctx, params)
}

func (d *DockerOrchestrator) CloneVolumes(ctx context.Context, srcName, dstName string) error {
	// Stop destination container while we copy data into its volumes
	timeout := 30
	d.client.ContainerStop(ctx, dstName, container.StopOptions{Timeout: &timeout})

	for _, suffix := range volumeSuffixes {
		srcVol := d.volumeName(srcName, suffix)
		dstVol := d.volumeName(dstName, suffix)
		if err := d.copyVolume(ctx, srcVol, dstVol); err != nil {
			// Best-effort: restart destination even on error
			d.client.ContainerStart(ctx, dstName, container.StartOptions{})
			return fmt.Errorf("copy volume %s: %w", suffix, err)
		}
	}

	return d.client.ContainerStart(ctx, dstName, container.StartOptions{})
}

func (d *DockerOrchestrator) copyVolume(ctx context.Context, srcVol, dstVol string) error {
	_ = d.ensureImage(ctx, "alpine:latest")

	containerCfg := &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"sh", "-c", "cp -a /src/. /dst/"},
	}
	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: srcVol, Target: "/src", ReadOnly: true},
			{Type: mount.TypeVolume, Source: dstVol, Target: "/dst"},
		},
	}

	resp, err := d.client.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return fmt.Errorf("create copy container: %w", err)
	}
	defer d.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := d.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start copy container: %w", err)
	}

	statusCh, errCh := d.client.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("wait for copy container: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("copy failed with exit code %d", status.StatusCode)
		}
	}
	return nil
}

func (d *DockerOrchestrator) DeleteInstance(ctx context.Context, name string) error {
	// Remove container
	err := d.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	if err != nil && !dockerclient.IsErrNotFound(err) {
		log.Printf("Remove container %s: %v", utils.SanitizeForLog(name), err)
	}

	// Remove volumes
	for _, suffix := range volumeSuffixes {
		volName := d.volumeName(name, suffix)
		if err := d.client.VolumeRemove(ctx, volName, true); err != nil && !dockerclient.IsErrNotFound(err) {
			log.Printf("Remove volume %s: %v", utils.SanitizeForLog(volName), err)
		}
	}
	return nil
}

func (d *DockerOrchestrator) DeleteSharedVolume(ctx context.Context, folderID uint) error {
	volName := fmt.Sprintf("claworc-shared-%d", folderID)
	if err := d.client.VolumeRemove(ctx, volName, true); err != nil && !dockerclient.IsErrNotFound(err) {
		return fmt.Errorf("remove shared volume %s: %w", volName, err)
	}
	return nil
}

func (d *DockerOrchestrator) StartInstance(ctx context.Context, name string) error {
	return d.client.ContainerStart(ctx, name, container.StartOptions{})
}

func (d *DockerOrchestrator) StopInstance(ctx context.Context, name string) error {
	timeout := 30
	return d.client.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout})
}

func (d *DockerOrchestrator) RestartInstance(ctx context.Context, name string, params CreateParams) error {
	// Stop and remove the container, then recreate it so mount changes take effect
	timeout := 30
	if err := d.client.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop container %s: %w", name, err)
	}
	if err := d.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil && !dockerclient.IsErrNotFound(err) {
		return fmt.Errorf("remove container %s: %w", name, err)
	}

	// Ensure shared folder volumes exist
	d.ensureSharedVolumes(ctx, params.SharedFolderMounts)

	return d.createContainer(ctx, params)
}

// ensureSharedVolumes creates the managed Docker volumes backing each
// non-host-backed shared folder. Existing volumes are tolerated.
func (d *DockerOrchestrator) ensureSharedVolumes(ctx context.Context, mounts []SharedFolderMount) {
	for _, sfm := range mounts {
		// host-backed folders need no managed volume
		if sfm.HostPath != "" {
			continue
		}
		volName := fmt.Sprintf("claworc-shared-%d", sfm.VolumeID)
		_, err := d.client.VolumeCreate(ctx, volume.CreateOptions{
			Name:   volName,
			Labels: map[string]string{"managed-by": labelManagedBy, "type": "shared-folder"},
		})
		if err != nil {
			log.Printf("Shared volume %s may already exist: %v", volName, err)
		}
	}
}

func (d *DockerOrchestrator) UpdateImage(ctx context.Context, name string, params CreateParams) error {
	// Force-pull the latest image (bypass local cache)
	log.Printf("Force-pulling image %s for instance %s", params.ContainerImage, utils.SanitizeForLog(name))
	reader, err := d.client.ImagePull(ctx, params.ContainerImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", params.ContainerImage, err)
	}
	defer reader.Close()
	io.Copy(io.Discard, reader)
	log.Printf("Image %s pulled successfully", params.ContainerImage)

	// Stop and remove the old container (volumes are preserved)
	timeout := 30
	d.client.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout})
	if err := d.client.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil && !dockerclient.IsErrNotFound(err) {
		return fmt.Errorf("remove container %s: %w", name, err)
	}

	// Recreate the container with the same config but fresh image
	return d.createContainer(ctx, params)
}

// SelfUpdate pulls a fresh image for the control-plane's own container and
// hands off the stop/remove/recreate to a short-lived detached "updater"
// helper container launched via the same docker.sock the control-plane
// already has mounted (see docker-compose.yml / install.sh).
//
// Why a helper container instead of doing it in-process: the control-plane
// cannot safely ContainerStop+ContainerRemove its own container from inside
// itself. Stopping its own container delivers SIGTERM to its own PID 1 and
// tears down the very process that would otherwise go on to remove and
// recreate it - there is no way to guarantee the recreate step still runs
// after the container (and this goroutine) is killed. Delegating the swap
// to a separate, disposable container run from outside removes that race:
// the helper only depends on the Docker daemon, not on the control-plane
// process staying alive.
//
// The helper is given the exact `docker run` arguments needed to recreate
// the control-plane container (same name, image, ports, mounts, env,
// restart policy) read back from the *live* container's own inspect data,
// so no separate "how was I configured" bookkeeping is needed - whatever is
// actually running is what gets reproduced, just on the new image.
//
// image, when empty, means "same image reference as currently running".
// SelfUpdate always force-pulls to check for a newer digest, but only
// triggers the restart when the pulled image actually differs from what
// this container is running -- see the ID comparison below.
func (d *DockerOrchestrator) SelfUpdate(ctx context.Context, img string) (bool, error) {
	selfName := config.Cfg.SelfContainerName
	if selfName == "" {
		selfName = "claworc"
	}

	inspect, err := d.client.ContainerInspect(ctx, selfName)
	if err != nil {
		return false, fmt.Errorf("inspect self container %q: %w", selfName, err)
	}

	targetImage := img
	if targetImage == "" {
		targetImage = inspect.Config.Image
	}

	// runningImageID is the content-addressable ID of the image this
	// container was actually created from (resolved once at creation time,
	// not re-derived from the possibly-mutable tag on Config.Image).
	runningImageID := inspect.Image

	log.Printf("SelfUpdate: force-pulling image %s", targetImage)
	reader, err := d.client.ImagePull(ctx, targetImage, image.PullOptions{})
	if err != nil {
		return false, fmt.Errorf("pull image %s: %w", targetImage, err)
	}
	defer reader.Close()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return false, fmt.Errorf("pull image %s: %w", targetImage, err)
	}
	log.Printf("SelfUpdate: image %s pulled successfully", targetImage)

	// Compare the freshly-pulled image's ID against what's currently running.
	// A match means the tag was re-pulled but nothing actually changed (the
	// common "latest already is latest" case) -- skip the disruptive restart.
	// Any failure resolving the pulled image's ID is treated as "can't tell",
	// which fails open into restarting rather than silently doing nothing.
	pulledInfo, inspectErr := d.client.ImageInspect(ctx, targetImage)
	if inspectErr == nil && pulledInfo.ID != "" && pulledInfo.ID == runningImageID {
		log.Printf("SelfUpdate: pulled image %s matches the running image (%s); no restart needed", targetImage, shortImageID(runningImageID))
		return false, nil
	}
	if inspectErr != nil {
		log.Printf("SelfUpdate: could not inspect pulled image %s to compare digests (%v); proceeding with restart", targetImage, inspectErr)
	}

	// Build the exact docker-run invocation the helper will use to recreate
	// us, mirroring the live container's own config/host config.
	runArgs := selfUpdateRunArgs(selfName, targetImage, inspect)

	helperName := selfName + "-updater"
	// Best-effort: remove any stale helper from a previous failed attempt.
	_ = d.client.ContainerRemove(ctx, helperName, container.RemoveOptions{Force: true})

	// The helper script: wait for the current control-plane container to
	// actually stop (it exits on its own shortly after this handler returns
	// and the HTTP response is flushed, or the helper force-stops it after a
	// grace period), remove it, run the replacement, then remove itself
	// (AutoRemove below).
	script := selfUpdateHelperScript(selfName, runArgs)

	helperCfg := &container.Config{
		Image: "docker:cli", // small official image bundling the docker CLI
		Cmd:   []string{"sh", "-c", script},
		Labels: map[string]string{
			"managed-by": labelManagedBy,
			"purpose":    "self-update-helper",
		},
	}
	helperHostCfg := &container.HostConfig{
		Binds:         []string{"/var/run/docker.sock:/var/run/docker.sock"},
		AutoRemove:    true,
		NetworkMode:   "none",
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
	}

	// Ensure the helper image is present; a fresh host may not have pulled it yet.
	if _, _, err := d.client.ImageInspectWithRaw(ctx, "docker:cli"); err != nil {
		log.Printf("SelfUpdate: pulling helper image docker:cli")
		helperReader, perr := d.client.ImagePull(ctx, "docker:cli", image.PullOptions{})
		if perr != nil {
			return false, fmt.Errorf("pull helper image docker:cli: %w", perr)
		}
		io.Copy(io.Discard, helperReader)
		helperReader.Close()
	}

	resp, err := d.client.ContainerCreate(ctx, helperCfg, helperHostCfg, nil, nil, helperName)
	if err != nil {
		return false, fmt.Errorf("create updater helper: %w", err)
	}
	if err := d.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return false, fmt.Errorf("start updater helper: %w", err)
	}

	log.Printf("SelfUpdate: helper container %s launched; control-plane will restart shortly on image %s", helperName, targetImage)
	return true, nil
}

// shortImageID trims a full "sha256:..." image ID down to a short, log-
// friendly form (12 hex chars, matching `docker images` column width).
func shortImageID(id string) string {
	const prefix = "sha256:"
	trimmed := strings.TrimPrefix(id, prefix)
	if len(trimmed) > 12 {
		return trimmed[:12]
	}
	return trimmed
}

// selfUpdateRunArgs reconstructs the `docker run` argument list needed to
// recreate the control-plane container from its own live inspect data:
// same name, image, published ports, binds/mounts, env, network, labels,
// and restart policy.
//
// Labels matter more than they might look: a container started via
// `docker compose up` carries com.docker.compose.project/service/
// config-hash/etc. labels that compose uses (not the container name) to
// recognize "this is my container" on the next `docker compose up`.
// Without reproducing them here, a compose-managed control-plane would
// come back from a self-update as a plain, unlabeled container still named
// "claworc" -- and the next `docker compose up -d` would see the name
// already taken by a container it doesn't recognize as its own, rather than
// adopting it, and fail with a name conflict instead of reconciling.
// Reproducing every label the live container had (compose-added or
// otherwise) keeps the recreated container indistinguishable from one
// compose or install.sh would have created directly.
func selfUpdateRunArgs(name, img string, inspect types.ContainerJSON) []string {
	args := []string{"run", "-d", "--name", name, "--restart", "unless-stopped"}

	if inspect.HostConfig != nil {
		for _, b := range inspect.HostConfig.Binds {
			args = append(args, "-v", b)
		}
		for containerPort, bindings := range inspect.HostConfig.PortBindings {
			for _, b := range bindings {
				hostPort := b.HostPort
				if hostPort == "" {
					continue
				}
				spec := fmt.Sprintf("%s:%s", hostPort, containerPort.Port())
				if b.HostIP != "" {
					spec = fmt.Sprintf("%s:%s:%s", b.HostIP, hostPort, containerPort.Port())
				}
				args = append(args, "-p", spec)
			}
		}
	}
	if inspect.NetworkSettings != nil {
		for netName := range inspect.NetworkSettings.Networks {
			args = append(args, "--network", netName)
			break // docker run only accepts one network at creation time; this backend always attaches exactly one
		}
	}
	if inspect.Config != nil {
		for _, e := range inspect.Config.Env {
			args = append(args, "-e", e)
		}
		// Sort keys for deterministic output (map iteration order is random in
		// Go; stable args make the generated script/log line reproducible and
		// easy to diff between runs).
		labelKeys := make([]string, 0, len(inspect.Config.Labels))
		for k := range inspect.Config.Labels {
			labelKeys = append(labelKeys, k)
		}
		sort.Strings(labelKeys)
		for _, k := range labelKeys {
			args = append(args, "--label", fmt.Sprintf("%s=%s", k, inspect.Config.Labels[k]))
		}
	}
	args = append(args, img)
	return args
}

// selfUpdateHelperScript is the shell script run inside the disposable
// docker:cli helper container. It waits for the named container to stop
// (the control-plane exits on its own shortly after SelfUpdate returns and
// the HTTP response is flushed - see handlers.SelfUpdateControlPlane), then
// removes it and recreates it via the given docker-run args. A bounded wait
// with a fallback force-stop guards against the caller/process not exiting
// promptly for any reason.
func selfUpdateHelperScript(name string, runArgs []string) string {
	quoted := make([]string, len(runArgs))
	for i, a := range runArgs {
		quoted[i] = shellQuote(a)
	}
	return fmt.Sprintf(`set -e
NAME=%s
for i in $(seq 1 30); do
  STATUS=$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null || echo "gone")
  if [ "$STATUS" != "true" ]; then break; fi
  sleep 1
done
docker stop "$NAME" >/dev/null 2>&1 || true
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker %s
`, shellQuote(name), strings.Join(quoted, " "))
}

// shellQuote wraps s in single quotes for safe inclusion in the sh -c
// script above, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// createContainer builds and starts a container from CreateParams (without pulling or creating volumes).
func (d *DockerOrchestrator) createContainer(ctx context.Context, params CreateParams) error {
	var env []string
	if parts := strings.SplitN(params.VNCResolution, "x", 2); len(parts) == 2 {
		env = append(env, "DISPLAY_WIDTH="+parts[0], "DISPLAY_HEIGHT="+parts[1])
	}
	for k, v := range params.EnvVars {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	if params.Timezone != "" {
		env = append(env, fmt.Sprintf("TZ=%s", params.Timezone))
	}
	if params.UserAgent != "" {
		env = append(env, fmt.Sprintf("CHROMIUM_USER_AGENT=%s", params.UserAgent))
	}

	mounts := []mount.Mount{
		{Type: mount.TypeVolume, Source: d.volumeName(params.Name, "homebrew"), Target: "/home/linuxbrew/.linuxbrew"},
		{Type: mount.TypeVolume, Source: d.volumeName(params.Name, "home"), Target: "/home/claworc"},
	}
	for _, sfm := range params.SharedFolderMounts {
		if sfm.HostPath != "" {
			// Create the host directory (recursively) so the bind mount can
			// attach. Only paths within the operator allowlist are created, so
			// a stale/shrunken allowlist can never auto-create arbitrary dirs.
			if err := ensureHostBindDir(sfm.HostPath); err != nil {
				return fmt.Errorf("prepare host mount %q: %w", sfm.HostPath, err)
			}
			mounts = append(mounts, mount.Mount{
				Type:     mount.TypeBind,
				Source:   sfm.HostPath,
				Target:   sfm.MountPath,
				ReadOnly: sfm.ReadOnly,
			})
			continue
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: fmt.Sprintf("claworc-shared-%d", sfm.VolumeID),
			Target: sfm.MountPath,
		})
	}

	var nanoCPUs int64
	var memLimit int64
	if params.CPULimit != "" {
		nanoCPUs = parseCPUToNanoCPUs(params.CPULimit)
	}
	if params.MemoryLimit != "" {
		memLimit = parseMemoryToBytes(params.MemoryLimit)
	}

	shmSize, _ := units.RAMInBytes("2g")

	containerCfg := &container.Config{
		Image:    params.ContainerImage,
		Hostname: strings.TrimPrefix(params.Name, "bot-"),
		Env:      env,
		Labels:   map[string]string{"managed-by": labelManagedBy, "instance": params.Name},
		ExposedPorts: nat.PortSet{
			"22/tcp": struct{}{},
		},
		Healthcheck: &container.HealthConfig{
			Test:          []string{"CMD-SHELL", "bash -c '>/dev/tcp/127.0.0.1/22'"},
			Interval:      30_000_000_000,
			Timeout:       10_000_000_000,
			Retries:       3,
			StartInterval: 60_000_000_000,
		},
	}

	hostCfg := &container.HostConfig{
		Privileged: false,
		Mounts:     mounts,
		ShmSize:    shmSize,
		Resources: container.Resources{
			NanoCPUs: nanoCPUs,
			Memory:   memLimit,
		},
		PortBindings: nat.PortMap{
			"22/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			networkName: {},
		},
	}

	resp, err := d.client.ContainerCreate(ctx, containerCfg, hostCfg, netCfg, nil, params.Name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	if err := d.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}

	// Fix ownership of shared folder mounts so the claworc user (1000:1000) can write to them.
	// New Docker volumes are owned by root by default. Host-backed bind mounts are
	// skipped — we never modify ownership of the operator's host directories.
	for _, sfm := range params.SharedFolderMounts {
		if sfm.HostPath != "" {
			continue
		}
		execCfg := container.ExecOptions{
			Cmd: []string{"chown", "claworc:claworc", sfm.MountPath},
		}
		idResp, err := d.client.ContainerExecCreate(ctx, resp.ID, execCfg)
		if err != nil {
			log.Printf("Failed to create chown exec for %s: %v", sfm.MountPath, err)
			continue
		}
		if err := d.client.ContainerExecStart(ctx, idResp.ID, container.ExecStartOptions{}); err != nil {
			log.Printf("Failed to chown %s: %v", sfm.MountPath, err)
		}
	}

	return nil
}

// GetInstanceEnv returns the running container's environment. Config.Env also
// carries whatever the image baked in, which Claworc does not own -- callers
// compare only the keys they expect to be there rather than the whole map.
func (d *DockerOrchestrator) GetInstanceEnv(ctx context.Context, name string) (map[string]string, error) {
	inspect, err := d.client.ContainerInspect(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", name, err)
	}
	env := map[string]string{}
	if inspect.Config == nil {
		return env, nil
	}
	for _, kv := range inspect.Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[k] = v
	}
	return env, nil
}

func (d *DockerOrchestrator) GetInstanceStatus(ctx context.Context, name string) (string, error) {
	inspect, err := d.client.ContainerInspect(ctx, name)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			return "stopped", nil
		}
		return "error", nil
	}

	status := inspect.State.Status
	health := ""
	if inspect.State.Health != nil {
		health = inspect.State.Health.Status
	}

	switch status {
	case "running":
		switch health {
		case "healthy":
			return "running", nil
		case "unhealthy":
			return "error", nil
		default:
			return "creating", nil
		}
	case "created", "restarting":
		return "creating", nil
	case "exited", "dead", "paused", "removing":
		return "stopped", nil
	default:
		return "stopped", nil
	}
}

func (d *DockerOrchestrator) GetInstanceImageInfo(ctx context.Context, name string) (string, error) {
	inspect, err := d.client.ContainerInspect(ctx, name)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect container: %w", err)
	}
	tag := inspect.Config.Image
	sha := inspect.Image
	if len(sha) > 19 { // "sha256:" (7) + 12 chars
		sha = sha[:19]
	}
	return fmt.Sprintf("%s (%s)", tag, sha), nil
}

func (d *DockerOrchestrator) ConfigureSSHAccess(ctx context.Context, instanceID uint, publicKey string) error {
	var inst database.Instance
	if err := database.DB.First(&inst, instanceID).Error; err != nil {
		return fmt.Errorf("instance %d not found: %w", instanceID, err)
	}
	return configureSSHAccess(ctx, d.ExecInInstance, inst.Name, publicKey)
}

func (d *DockerOrchestrator) GetSSHAddress(ctx context.Context, instanceID uint) (string, int, error) {
	var inst database.Instance
	if err := database.DB.First(&inst, instanceID).Error; err != nil {
		return "", 0, fmt.Errorf("instance %d not found: %w", instanceID, err)
	}
	inspect, err := d.client.ContainerInspect(ctx, inst.Name)
	if err != nil {
		return "", 0, fmt.Errorf("inspect container for instance %d: %w", instanceID, err)
	}

	// Detect whether the control-plane itself is running inside a Docker container.
	// /.dockerenv is created by the Docker runtime in every container.
	runningInDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		runningInDocker = true
	}

	// Inside Docker: use the container IP on the claworc bridge network for
	// direct container-to-container communication (no port mapping needed).
	if runningInDocker {
		if ep, ok := inspect.NetworkSettings.Networks[networkName]; ok && ep.IPAddress != "" {
			return ep.IPAddress, 22, nil
		}
	}

	// On the host (e.g. macOS / Windows): Docker bridge IPs are not routable
	// from the host OS, so use the published host port on the loopback instead.
	if bindings, ok := inspect.NetworkSettings.Ports["22/tcp"]; ok && len(bindings) > 0 {
		port := 0
		fmt.Sscanf(bindings[0].HostPort, "%d", &port)
		if port > 0 {
			return "127.0.0.1", port, nil
		}
	}

	// Fallback: on Linux hosts bridge IPs are routable from the host, so the
	// container IP still works even when we're not inside Docker ourselves.
	if ep, ok := inspect.NetworkSettings.Networks[networkName]; ok && ep.IPAddress != "" {
		return ep.IPAddress, 22, nil
	}

	return "", 0, fmt.Errorf("cannot determine SSH address for instance %d", instanceID)
}

func (d *DockerOrchestrator) UpdatePlacementConfig(_ context.Context, name string, _ UpdatePlacementParams) error {
	log.Printf("UpdatePlacementConfig: Docker orchestrator does not support pod placement for %s; ignoring", name)
	return nil
}

func (d *DockerOrchestrator) UpdateResources(ctx context.Context, name string, params UpdateResourcesParams) error {
	updateCfg := container.UpdateConfig{
		Resources: container.Resources{
			NanoCPUs: parseCPUToNanoCPUs(params.CPULimit),
			Memory:   parseMemoryToBytes(params.MemoryLimit),
		},
	}
	_, err := d.client.ContainerUpdate(ctx, name, updateCfg)
	return err
}

func (d *DockerOrchestrator) GetContainerStats(ctx context.Context, name string) (*ContainerStats, error) {
	resp, err := d.client.ContainerStatsOneShot(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("container stats: %w", err)
	}
	defer resp.Body.Close()

	var statsJSON dockerStatsJSON
	if err := json.NewDecoder(resp.Body).Decode(&statsJSON); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}

	// CPU usage calculation (same formula as docker stats CLI)
	cpuDelta := float64(statsJSON.CPUStats.CPUUsage.TotalUsage - statsJSON.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(statsJSON.CPUStats.SystemCPUUsage - statsJSON.PreCPUStats.SystemCPUUsage)
	numCPUs := float64(statsJSON.CPUStats.OnlineCPUs)
	if numCPUs == 0 {
		numCPUs = float64(len(statsJSON.CPUStats.CPUUsage.PercpuUsage))
	}

	var cpuCores float64
	if systemDelta > 0 && numCPUs > 0 {
		cpuCores = (cpuDelta / systemDelta) * numCPUs
	}
	cpuMillicores := int64(cpuCores * 1000)

	memUsage := statsJSON.MemoryStats.Usage
	memLimit := statsJSON.MemoryStats.Limit

	var cpuPercent float64
	if memLimit > 0 && statsJSON.CPUStats.CPUUsage.TotalUsage > 0 {
		// Calculate CPU % of limit using NanoCPUs from container config
		inspect, err := d.client.ContainerInspect(ctx, name)
		if err == nil && inspect.HostConfig.NanoCPUs > 0 {
			limitCores := float64(inspect.HostConfig.NanoCPUs) / 1e9
			cpuPercent = (cpuCores / limitCores) * 100
		}
	}

	return &ContainerStats{
		CPUUsageMillicores: cpuMillicores,
		CPUUsagePercent:    cpuPercent,
		MemoryUsageBytes:   int64(memUsage),
		MemoryLimitBytes:   int64(memLimit),
	}, nil
}

type dockerStatsJSON struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
}

func (d *DockerOrchestrator) UpdateInstanceConfig(ctx context.Context, name string, configJSON string) error {
	return updateInstanceConfig(ctx, d.ExecInInstance, d.InstanceFactory, name, configJSON)
}

func stripDockerLogHeaders(data []byte) string {
	// Docker multiplexed log format: [stream_type(1)][0(3)][size(4)][payload]
	// If the data starts with a valid header byte (0, 1, or 2), try to strip
	var result strings.Builder
	for len(data) > 0 {
		if len(data) >= 8 && (data[0] == 0 || data[0] == 1 || data[0] == 2) {
			size := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
			data = data[8:]
			if size > 0 && size <= len(data) {
				result.Write(data[:size])
				data = data[size:]
			} else {
				result.Write(data)
				break
			}
		} else {
			result.Write(data)
			break
		}
	}
	return result.String()
}

func (d *DockerOrchestrator) ExecInInstance(ctx context.Context, name string, cmd []string) (string, string, int, error) {
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := d.client.ContainerExecCreate(ctx, name, execCfg)
	if err != nil {
		return "", "", -1, fmt.Errorf("exec create: %w", err)
	}

	resp, err := d.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", "", -1, fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	output, err := io.ReadAll(resp.Reader)
	if err != nil {
		return "", "", -1, fmt.Errorf("read exec output: %w", err)
	}

	// Get exit code
	inspectResp, err := d.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return string(output), "", -1, fmt.Errorf("exec inspect: %w", err)
	}

	// Docker exec with demux=false returns multiplexed output
	// For simplicity, treat all output as stdout
	cleaned := stripDockerLogHeaders(output)
	return cleaned, "", inspectResp.ExitCode, nil
}

func (d *DockerOrchestrator) StreamExecInInstance(ctx context.Context, name string, cmd []string, stdout io.Writer) (string, int, error) {
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}

	execID, err := d.client.ContainerExecCreate(ctx, name, execCfg)
	if err != nil {
		return "", -1, fmt.Errorf("exec create: %w", err)
	}

	resp, err := d.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", -1, fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	var stderrBuf strings.Builder
	if err := demuxDockerStream(resp.Reader, stdout, &stderrBuf); err != nil {
		return stderrBuf.String(), -1, fmt.Errorf("stream exec output: %w", err)
	}

	inspectResp, err := d.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return stderrBuf.String(), -1, fmt.Errorf("exec inspect: %w", err)
	}

	return stderrBuf.String(), inspectResp.ExitCode, nil
}

// demuxDockerStream reads Docker's multiplexed stream format and routes
// stdout (stream type 1) to stdoutW and stderr (stream type 2) to stderrW.
func demuxDockerStream(reader io.Reader, stdoutW io.Writer, stderrW io.Writer) error {
	header := make([]byte, 8)
	for {
		_, err := io.ReadFull(reader, header)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		streamType := header[0]
		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		if size == 0 {
			continue
		}
		var dst io.Writer
		switch streamType {
		case 1:
			dst = stdoutW
		case 2:
			dst = stderrW
		default:
			dst = stdoutW
		}
		if _, err := io.CopyN(dst, reader, int64(size)); err != nil {
			return err
		}
	}
}

// Ensure DockerOrchestrator implements ContainerOrchestrator
var _ ContainerOrchestrator = (*DockerOrchestrator)(nil)
