package control

import (
	"github.com/containerd/containerd/snapshot"
	controlapi "github.com/tonistiigi/buildkit_poc/api/services/control"
	"github.com/tonistiigi/buildkit_poc/cache"
	"github.com/tonistiigi/buildkit_poc/solver"
	"github.com/tonistiigi/buildkit_poc/source"
	"github.com/tonistiigi/buildkit_poc/worker"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type Opt struct {
	Snapshotter   snapshot.Snapshotter
	CacheManager  cache.Manager
	Worker        worker.Worker
	SourceManager *source.Manager
}

type Controller struct { // TODO: ControlService
	opt    Opt
	solver *solver.Solver
}

func NewController(opt Opt) (*Controller, error) {
	c := &Controller{
		opt: opt,
		solver: solver.New(solver.Opt{
			SourceManager: opt.SourceManager,
			CacheManager:  opt.CacheManager,
			Worker:        opt.Worker,
		}),
	}
	return c, nil
}

func (c *Controller) Register(server *grpc.Server) error {
	controlapi.RegisterControlServer(server, c)
	return nil
}

func (c *Controller) DiskUsage(ctx context.Context, _ *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	du, err := c.opt.CacheManager.DiskUsage(ctx)
	if err != nil {
		return nil, err
	}

	resp := &controlapi.DiskUsageResponse{}
	for _, r := range du {
		resp.Record = append(resp.Record, &controlapi.UsageRecord{
			ID:      r.ID,
			Mutable: r.Mutable,
			InUse:   r.InUse,
			Size_:   r.Size,
		})
	}
	return resp, nil
}

func (c *Controller) Solve(ctx context.Context, req *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	v, err := solver.Load(req.Definition)
	if err != nil {
		return nil, err
	}
	if err := c.solver.Solve(ctx, v); err != nil {
		return nil, err
	}
	return &controlapi.SolveResponse{}, nil
}
