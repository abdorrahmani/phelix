package deploy

import (
	"context"
	"os"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// KnownContainerIDs returns the container ids a DeployState currently accounts
// for (blue-green slots + rolling replicas). Orphan reconciliation uses the
// union of these across all apps as the allow-list: any phelix.managed
// container NOT in that set was left behind by a crashed deploy and is safe to
// reclaim.
func KnownContainerIDs(state *DeployState) []string {
	if state == nil {
		return nil
	}
	var ids []string
	if state.ScaleProgress != nil {
		if state.ScaleProgress.NewContainerID != "" {
			ids = append(ids, state.ScaleProgress.NewContainerID)
		}
		if state.ScaleProgress.VictimContainerID != "" {
			ids = append(ids, state.ScaleProgress.VictimContainerID)
		}
	}
	for _, inst := range state.Retiring {
		if inst != nil && inst.ContainerID != "" {
			ids = append(ids, inst.ContainerID)
		}
	}
	for _, inst := range state.Slots {
		if inst != nil && inst.ContainerID != "" {
			ids = append(ids, inst.ContainerID)
		}
	}
	for _, inst := range state.Replicas {
		if inst != nil && inst.ContainerID != "" {
			ids = append(ids, inst.ContainerID)
		}
	}
	return ids
}

// ReconcileOrphanContainers force-removes every phelix.managed container that
// no DeployState accounts for. It is the container-runtime analogue of the
// process-based stale-slot recovery: a deploy that crashed after `docker run`
// but before persisting the new instance leaves a running container that no
// state references and the proxy never routes to. Left alone it wastes the
// host's resources and holds a published port; this reclaims it on the next
// daemon start.
//
// Safety: only containers carrying the phelix.managed=true label are ever
// considered, and only those whose full id is absent from known are removed —
// so a container Phelix did not create, or one still tracked by state, is never
// touched. This mirrors findVerifiedProcess's "verify identity before acting"
// rule for the process world.
//
// known holds the full container ids every app's state still references.
func ReconcileOrphanContainers(ctx context.Context, appName string, known map[string]bool) ([]string, error) {
	if err := ValidateAppName(appName); err != nil {
		return nil, err
	}
	return reconcileOrphanContainers(ctx, appName, known, execDockerRunner)
}

func reconcileOrphanContainers(ctx context.Context, appName string, known map[string]bool, run dockerRunner) ([]string, error) {
	args := []string{"ps", "-a", "--no-trunc",
		"--filter", "label=" + DockerLabelManaged + "=true",
		"--filter", "label=" + DockerLabelApp + "=" + appName,
		"--format", "{{.ID}}"}
	if owner := os.Getenv("PHELIX_CONTROLLER_ID"); owner != "" {
		args = append(args, "--filter", "label="+DockerLabelController+"="+owner)
	}
	out, err := run(ctx, args...)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeDocker, err, "deploy: list phelix-managed containers")
	}

	var removed []string
	for _, id := range strings.Split(strings.TrimSpace(out), "\n") {
		id = strings.TrimSpace(id)
		if id == "" || known[id] {
			continue
		}

		owned, verifyErr := ownedDockerContainer(ctx, run, id)
		if verifyErr != nil {
			return removed, verifyErr
		}
		if !owned {
			continue
		}
		if _, rmErr := run(ctx, "rm", "-f", id); rmErr != nil {
			continue
		}
		removed = append(removed, id)
	}
	return removed, nil
}
