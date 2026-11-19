package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/server"
)

type Controller struct {
	Config   Config
	Identity string
	owner    *Owner
}

func New(ctx context.Context, cfg Config) (*Controller, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owner, err := Acquire(server.DataDir())
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			owner.Close()
		}
	}()
	if err := MigrateStorage(filepath.Join(os.Getenv("HOME"), ".phelix"), server.DataDir()); err != nil {
		return nil, err
	}
	if err := app.Manager.LoadState(); err != nil {
		return nil, err
	}
	id, err := server.EnsureAgentID()
	if err != nil {
		return nil, err
	}
	success = true
	return &Controller{Config: cfg, Identity: id, owner: owner}, nil
}

func (c *Controller) Close() error {
	return errors.Join(c.owner.Close())
}
