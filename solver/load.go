package solver

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"

	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/tonistiigi/buildkit_poc/cache"
	"github.com/tonistiigi/buildkit_poc/solver/pb"
	"github.com/tonistiigi/buildkit_poc/source"
	"github.com/tonistiigi/buildkit_poc/worker"
)

type opVertex struct {
	mu     sync.Mutex
	op     *pb.Op
	inputs []*opVertex
	refs   []cache.ImmutableRef
	err    error
}

func Load(ops [][]byte) (*opVertex, error) {
	m := make(map[digest.Digest]*pb.Op)

	var lastOp *pb.Op
	var dgst digest.Digest

	for i, dt := range ops {
		var op pb.Op
		if err := (&op).Unmarshal(dt); err != nil {
			return nil, errors.Wrap(err, "failed to parse op")
		}
		lastOp = &op
		dgst = digest.FromBytes(dt)
		if i != len(ops)-1 {
			m[dgst] = &op
		}
	}

	cache := make(map[digest.Digest]*opVertex)

	// TODO: validate the connections
	vtx, err := loadReqursive(dgst, lastOp, m, cache)
	if err != nil {
		return nil, err
	}

	return vtx, err
}

func loadReqursive(dgst digest.Digest, op *pb.Op, inputs map[digest.Digest]*pb.Op, cache map[digest.Digest]*opVertex) (*opVertex, error) {
	if v, ok := cache[dgst]; ok {
		return v, nil
	}
	vtx := &opVertex{op: op}
	for _, in := range op.Inputs {
		op, ok := inputs[digest.Digest(in)]
		if !ok {
			return nil, errors.Errorf("failed to find %s", in)
		}
		sub, err := loadReqursive(digest.Digest(in), op, inputs, cache)
		if err != nil {
			return nil, err
		}
		vtx.inputs = append(vtx.inputs, sub)
	}
	cache[dgst] = vtx
	return vtx, nil
}

type SolverOpt struct {
	SourceManager source.Manager
	CacheManager  cache.Manager // TODO: this shouldn't be needed before instruction cache
	Worker        worker.Worker
}

func (g *opVertex) inputRequiresExport(i int) bool {
	return true // TODO
}

type Solver struct {
	opt SolverOpt
}

func NewSolver(opt SolverOpt) *Solver {
	return &Solver{opt: opt}
}

func (s *Solver) Solve(ctx context.Context, g *opVertex) error {
	err := g.solve(ctx, s.opt) // TODO: separate exporting
	return err
}

func (g *opVertex) release(ctx context.Context) error {
	return errors.Errorf("release not implemented")
}

func (g *opVertex) solve(ctx context.Context, opt SolverOpt) (retErr error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.err != nil {
		return g.err
	}
	if len(g.refs) > 0 {
		return nil
	}

	defer func() {
		if retErr != nil {
			g.err = retErr
		}
	}()

	if len(g.inputs) > 0 {
		eg, ctx := errgroup.WithContext(ctx)

		for _, in := range g.inputs {
			eg.Go(func() error {
				err := in.solve(ctx, opt)
				if err != nil {
					return err
				}
				return nil
			})
		}
		err := eg.Wait()
		if err != nil {
			return err
		}
	}

	switch op := g.op.Op.(type) {
	case *pb.Op_Source:
		id, err := source.FromString(op.Source.Identifier)
		if err != nil {
			return err
		}
		ref, err := opt.SourceManager.Pull(ctx, id)
		if err != nil {
			return err
		}
		g.refs = []cache.ImmutableRef{ref}
	case *pb.Op_Exec:

		mounts := make(map[string]cache.Mountable)

		var outputs []cache.MutableRef

		defer func() {
			for _, o := range outputs {
				if o != nil {
					s, err := o.Freeze() // TODO: log error
					if err == nil {
						s.Release()
					}
				}
			}
		}()

		for _, m := range op.Exec.Mounts {
			var mountable cache.Mountable
			ref := g.refs[int(m.Input)]
			mountable = ref
			if m.Output != -1 {
				active, err := opt.CacheManager.New(ref) // TODO: should be method
				if err != nil {
					return err
				}
				outputs = append(outputs, active)
				mountable = active
			}
			mounts[m.Dest] = mountable
		}

		meta := worker.Meta{
			Args: op.Exec.Meta.Args,
			Env:  op.Exec.Meta.Env,
			Cwd:  op.Exec.Meta.Cwd,
		}

		if err := opt.Worker.Exec(ctx, meta, mounts, nil, nil); err != nil {
			return errors.Wrapf(err, "worker failed running %v", meta.Args)
		}

		g.refs = []cache.ImmutableRef{}

		for i, o := range outputs {
			ref, err := o.ReleaseAndCommit(ctx)
			if err != nil {
				return errors.Wrapf(err, "error committing %s", ref.ID())
			}
			g.refs = append(g.refs, ref)
			outputs[i] = nil
		}

	default:
		return errors.Errorf("invalid op type")
	}
	return nil
}
