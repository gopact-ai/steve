package fleetlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// A failed docker command may have created its named resource already.
// Remove every recorded name and report failures instead of hiding leaks.
func (d *docker) remove() error {
	var errs []error
	for _, name := range d.containers {
		if out, err := run(time.Minute, "docker", "rm", "--force", "--volumes", name); err != nil && !missingDockerResource(out) {
			errs = append(errs, fmt.Errorf("remove container %s: %w: %s", name, err, out))
		}
	}
	if d.network != "" {
		if out, err := run(time.Minute, "docker", "network", "rm", d.network); err != nil && !missingDockerResource(out) {
			errs = append(errs, fmt.Errorf("remove network %s: %w: %s", d.network, err, out))
		}
	}
	if d.work != "" {
		errs = append(errs, os.RemoveAll(d.work))
	}
	return errors.Join(errs...)
}

func missingDockerResource(out string) bool {
	return strings.Contains(out, "No such container:") || strings.Contains(out, "No such network:") ||
		(strings.Contains(out, "Error response from daemon: network ") && strings.Contains(out, " not found"))
}

// Finish creation before cleaning up its name. Killing the Docker client on
// cancellation could leave the daemon creating a resource after removal ran.
// These mutations have their own deadline; cancellation is returned afterwards.
func runDockerMutation(ctx context.Context, within time.Duration, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := runContext(context.WithoutCancel(ctx), within, "docker", args...)
	return out, errors.Join(err, ctx.Err())
}
