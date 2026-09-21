package deploy

// Runtime names how a deploy launches instances. Kept as string constants
// (mirroring project.RuntimeNative/RuntimeDocker) so internal/deploy stays free
// of an import cycle on internal/project — the CLI passes the resolved value in.
const (
	RuntimeNative = "native"
	RuntimeDocker = "docker"
)

// IsDockerRuntime reports whether r selects the container runtime. Empty and
// any unrecognized value mean native, so every pre-docker DeployState (which
// has no Runtime field on disk) resolves to the native path unchanged.
func IsDockerRuntime(r string) bool {
	return r == RuntimeDocker
}

// LauncherForRuntime returns the InstanceLauncher a deploy should use for the
// given runtime. Native returns the existing binary launcher (unchanged
// behavior); docker returns the container launcher. Centralized here so the
// forward deploy, rollback, and auto-rollback paths never diverge on which
// launcher a runtime maps to.
func LauncherForRuntime(runtime, appName string) InstanceLauncher {
	if IsDockerRuntime(runtime) {
		return DockerLauncherForApp(appName)
	}
	return LauncherForApp(appName)
}

// RollbackSourceForRuntime returns the BuildSource a rollback should resolve its
// target from, matched to the runtime the app was deployed with. Docker apps
// roll back to a recorded image reference (DockerVersionSource); native apps to
// an on-disk binary (ExistingVersionSource). version 0 means "previous".
//
// This is what makes rollback runtime-correct without the caller having to
// know: ExecuteRollback reads state.Runtime and calls this, so a docker app can
// never be rolled back by trying to exec an image reference as a file path.
func RollbackSourceForRuntime(runtime, appName, appID string, version int) BuildSource {
	if IsDockerRuntime(runtime) {
		return &DockerVersionSource{AppName: appName, AppID: appID, Version: version}
	}
	return &ExistingVersionSource{AppName: appName, Version: version}
}
