package docker

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"time"
)

// RootDirTTL is how long DockerRootDir reuses the data root it read.
const RootDirTTL = time.Minute

// Info is the daemon's system information, reduced to the fields the
// companion uses.
type Info struct {
	// DockerRootDir is the daemon's data root, holding every container's
	// file system, image and volume: /var/lib/docker unless the daemon is
	// configured with another data-root.
	DockerRootDir string `json:"DockerRootDir"`
}

// Info returns the daemon's system information.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.do(ctx, http.MethodGet, "/info", nil, nil, &out)
	return out, err
}

// DockerRootDir returns the daemon's data root, as a clean absolute path.
// It reads /info at most once per RootDirTTL: concurrent callers wait for
// one read and share it. A failed read is not cached, and an expired value
// is never returned in its place, so callers judging host access fail
// closed instead of trusting a data root the daemon may have changed.
func (c *Client) DockerRootDir(ctx context.Context) (string, error) {
	select {
	case c.rootDirSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.rootDirSem }()
	if c.rootDir != "" && c.now().Sub(c.rootDirAt) < RootDirTTL {
		return c.rootDir, nil
	}
	info, err := c.Info(ctx)
	if err != nil {
		return "", err
	}
	if !path.IsAbs(info.DockerRootDir) {
		return "", fmt.Errorf("docker: the daemon reports data root %q, not an absolute path", info.DockerRootDir)
	}
	c.rootDir, c.rootDirAt = path.Clean(info.DockerRootDir), c.now()
	return c.rootDir, nil
}
