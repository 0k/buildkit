package solver

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/tonistiigi/buildkit_poc/cache"
	"github.com/tonistiigi/buildkit_poc/client"
	"github.com/tonistiigi/buildkit_poc/identity"
	"github.com/tonistiigi/buildkit_poc/solver/pb"
	"github.com/tonistiigi/buildkit_poc/source"
	"github.com/tonistiigi/buildkit_poc/util/progress"
	"github.com/tonistiigi/buildkit_poc/worker"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

type Opt struct {
	SourceManager *source.Manager
	CacheManager  cache.Manager // TODO: this shouldn't be needed before instruction cache
	Worker        worker.Worker
}

type Solver struct {
	opt  Opt
	jobs *jobList
}

func New(opt Opt) *Solver {
	return &Solver{opt: opt, jobs: newJobList()}
}

func (s *Solver) Solve(ctx context.Context, id string, g *opVertex) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, ctx, closeProgressWriter := progress.NewContext(ctx)

	if len(g.inputs) > 0 { // TODO: detect op_return better
		g = g.inputs[0]
	}

	_, err := s.jobs.new(ctx, id, g, pr)
	if err != nil {
		return err
	}

	err = g.solve(ctx, s.opt) // TODO: separate exporting
	closeProgressWriter()
	if err != nil {
		return err
	}

	g.release(ctx)
	// TODO: export final vertex state
	return err
}

func (s *Solver) Status(ctx context.Context, id string, statusChan chan *client.SolveStatus) error {
	j, err := s.jobs.get(id)
	if err != nil {
		return err
	}
	defer close(statusChan)
	return j.pipe(ctx, statusChan)
}

type opVertex struct {
	mu     sync.Mutex
	op     *pb.Op
	inputs []*opVertex
	refs   []cache.ImmutableRef
	err    error
	dgst   digest.Digest
	vtx    client.Vertex
}

func (g *opVertex) inputRequiresExport(i int) bool {
	return true // TODO
}

func (g *opVertex) release(ctx context.Context) (retErr error) {
	for _, i := range g.inputs {
		if err := i.release(ctx); err != nil {
			retErr = err
		}
	}
	for _, ref := range g.refs {
		if ref != nil {
			if err := ref.Release(ctx); err != nil {
				retErr = err
			}
		}
	}
	return retErr
}

func (g *opVertex) getInputRefForIndex(i int) cache.ImmutableRef {
	input := g.op.Inputs[i]
	for _, v := range g.inputs {
		if v.dgst == digest.Digest(input.Digest) {
			return v.refs[input.Index]
		}
	}
	return nil
}

func (g *opVertex) solve(ctx context.Context, opt Opt) (retErr error) {
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

	pw, _, ctx := progress.FromContext(ctx, progress.WithMetadata("vertex", g.dgst))
	defer pw.Close()

	if len(g.inputs) > 0 {
		eg, ctx := errgroup.WithContext(ctx)

		for _, in := range g.inputs {
			func(in *opVertex) {
				eg.Go(func() error {
					if err := in.solve(ctx, opt); err != nil {
						return err
					}
					return nil
				})
			}(in)
		}
		err := eg.Wait()
		if err != nil {
			return err
		}
	}

	g.notifyStarted(ctx)
	defer g.notifyCompleted(ctx)

	switch op := g.op.Op.(type) {
	case *pb.Op_Source:
		if err := g.runSourceOp(ctx, opt.SourceManager, op); err != nil {
			return err
		}
	case *pb.Op_Exec:
		if err := g.runExecOp(ctx, opt.CacheManager, opt.Worker, op); err != nil {
			return err
		}
	default:
		return errors.Errorf("invalid op type %T", g.op.Op)
	}
	return nil
}

func (g *opVertex) runSourceOp(ctx context.Context, sm *source.Manager, op *pb.Op_Source) error {
	id, err := source.FromString(op.Source.Identifier)
	if err != nil {
		return err
	}
	ref, err := sm.Pull(ctx, id)
	if err != nil {
		return err
	}
	g.refs = []cache.ImmutableRef{ref}
	return nil
}

func (g *opVertex) runExecOp(ctx context.Context, cm cache.Manager, w worker.Worker, op *pb.Op_Exec) error {
	mounts := make(map[string]cache.Mountable)

	var outputs []cache.MutableRef

	defer func() {
		for _, o := range outputs {
			if o != nil {
				s, err := o.Freeze() // TODO: log error
				if err == nil {
					s.Release(ctx)
				}
			}
		}
	}()

	for _, m := range op.Exec.Mounts {
		var mountable cache.Mountable
		ref := g.getInputRefForIndex(int(m.Input))
		mountable = ref
		if m.Output != -1 {
			active, err := cm.New(ctx, ref) // TODO: should be method
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

	stdout := newStreamWriter(ctx, 1)
	defer stdout.Close()
	stderr := newStreamWriter(ctx, 2)
	defer stderr.Close()

	if err := w.Exec(ctx, meta, mounts, stdout, stderr); err != nil {
		return errors.Wrapf(err, "worker failed running %v", meta.Args)
	}

	g.refs = []cache.ImmutableRef{}
	for i, o := range outputs {
		ref, err := o.ReleaseAndCommit(ctx)
		if err != nil {
			return errors.Wrapf(err, "error committing %s", o.ID())
		}
		g.refs = append(g.refs, ref)
		outputs[i] = nil
	}
	return nil
}

func (g *opVertex) notifyStarted(ctx context.Context) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	g.vtx.Started = &now
	pw.Write(g.dgst.String(), g.vtx)
}

func (g *opVertex) notifyCompleted(ctx context.Context) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	g.vtx.Completed = &now
	pw.Write(g.dgst.String(), g.vtx)
}

func (g *opVertex) name() string {
	switch op := g.op.Op.(type) {
	case *pb.Op_Source:
		return op.Source.Identifier
	case *pb.Op_Exec:
		return strings.Join(op.Exec.Meta.Args, " ")
	default:
		return "unknown"
	}
}

func newStreamWriter(ctx context.Context, stream int) io.WriteCloser {
	pw, _, _ := progress.FromContext(ctx)
	return &streamWriter{
		pw:     pw,
		stream: stream,
	}
}

type streamWriter struct {
	pw     progress.Writer
	stream int
}

func (sw *streamWriter) Write(dt []byte) (int, error) {
	sw.pw.Write(identity.NewID(), client.VertexLog{
		Stream: sw.stream,
		Data:   append([]byte{}, dt...),
	})
	// TODO: remove debug
	switch sw.stream {
	case 1:
		return os.Stdout.Write(dt)
	case 2:
		return os.Stderr.Write(dt)
	default:
		return 0, errors.Errorf("invalid stream %d", sw.stream)
	}
}

func (sw *streamWriter) Close() error {
	return sw.pw.Close()
}
