package deploy

import (
	"context"
	"fmt"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DockerImageBuilder builds a container image for one deploy cycle and returns
// its reference (e.g. "app:v1"). Production wires this to the internal
// docker package's BuildImage; tests inject a fake that returns a ref without a
// daemon.
//
// version is the label the image will be tagged with (vN), resolved by
// DockerBuildSource before the build so the image ref and the versions.json row
// agree. It returns the exact image reference it tagged.
type DockerImageBuilder func(ctx context.Context, appName string, version int) (imageRef string, err error)

// DockerBuildSource is the container-runtime counterpart of FreshBuildSource.
// Instead of compiling a binary, it builds an image tagged <app>:vN, records
// the version with a DockerImage reference (is_current=false — same two-phase
// promotion), snapshots the encrypted env so a rollback restores the matching
// secrets, and returns the image ref in the binaryPath slot. The Docker
// InstanceLauncher interprets that slot as the image reference, so the whole
// downstream Deploy() flow (start → health → proxy switch → drain) is
// unchanged.
//
// The two-phase contract is preserved exactly: the version row is written with
// is_current=false here; PromoteVersion flips it only after the deploy's health
// check passes. An image that builds but never starts stays inspectable in
// versions.json and never becomes current.
type DockerBuildSource struct {
	AppName   string
	AppID     string
	GitCommit string
	Tag       string
	Retention RetentionPolicy
	Logger    Logger

	// BuildFn builds and tags the image. Required.
	BuildFn DockerImageBuilder

	lastVersion int
	lastImage   string
}

// Build resolves the next version, builds and tags <app>:vN, records the
// version (DockerImage ref, is_current=false), snapshots env/vN.enc, and
// returns (imageRef, envPath). imageRef travels in the binaryPath return slot
// that the Docker launcher reads as the image reference.
func (d *DockerBuildSource) Build(ctx context.Context) (string, string, error) {
	if d.BuildFn == nil {
		return "", "", phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: DockerBuildSource has no BuildFn")
	}

	// Resolve the version label BEFORE building so the image tag, the
	// versions.json row, and the env snapshot all reference the same vN.
	ver, err := NextVersion(d.AppName)
	if err != nil {
		return "", "", err
	}

	imageRef, err := d.BuildFn(ctx, d.AppName, ver)
	if err != nil {
		return "", "", phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "deploy: docker build v%d", ver)
	}
	if imageRef == "" {
		return "", "", phelixerr.New(phelixerr.CodeBuildFailed, "deploy: docker build returned an empty image reference")
	}

	// Record the version with is_current=false. RecordDockerBuild calls
	// NextVersion again internally; because no version is promoted between our
	// NextVersion() call and this one, both observe the same next integer.
	rec, err := RecordDockerBuild(d.AppName, imageRef, d.Tag, d.GitCommit, d.Retention, d.Logger)
	if err != nil {
		return "", "", err
	}

	// Pair an env snapshot with this version so a later rollback to vN restores
	// the same secrets — identical guarantee to the native path.
	envPath, err := snapshotEnvForVersion(d.AppName, d.AppID, rec.Version)
	if err != nil {
		return "", "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: snapshot env for docker v%d", rec.Version)
	}

	d.lastVersion = rec.Version
	d.lastImage = imageRef
	return imageRef, envPath, nil
}

func (d *DockerBuildSource) Describe() string {
	if d.lastVersion > 0 {
		return fmt.Sprintf("docker build v%d (%s)", d.lastVersion, d.lastImage)
	}
	return "docker build"
}

func (d *DockerBuildSource) TargetVersion() int { return d.lastVersion }

// DockerVersionSource resolves an already-built image version for rollback. It
// never rebuilds and never mutates versions.json — promotion happens only after
// Deploy succeeds, mirroring ExistingVersionSource for the native path.
type DockerVersionSource struct {
	AppName string
	AppID   string
	Version int // 0 = previous version
}

// Build returns the recorded image reference for the target version plus its
// paired env snapshot. The image ref rides in the binaryPath return slot.
func (d *DockerVersionSource) Build(_ context.Context) (string, string, error) {
	ver := d.Version
	if ver == 0 {
		prev, err := PreviousVersion(d.AppName)
		if err != nil {
			return "", "", err
		}
		ver = prev
	}

	imageRef, err := DockerImageForVersion(d.AppName, ver)
	if err != nil {
		return "", "", err
	}

	envPath, _ := envSnapshotPath(d.AppName, ver)
	// A missing env snapshot is tolerated (older versions may lack one); the
	// launcher simply starts the container with no overlay.
	return imageRef, envPath, nil
}

func (d *DockerVersionSource) Describe() string {
	if d.Version > 0 {
		return fmt.Sprintf("rollback to docker v%d", d.Version)
	}
	return "rollback to previous docker version"
}

func (d *DockerVersionSource) TargetVersion() int {
	if d.Version > 0 {
		return d.Version
	}
	v, err := PreviousVersion(d.AppName)
	if err != nil {
		return 0
	}
	return v
}
