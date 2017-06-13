package progress

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

type contextKeyT string

var contextKey = contextKeyT("buildkit/util/progress")

func FromContext(ctx context.Context, name string) (ProgressWriter, bool, context.Context) {
	pw, ok := ctx.Value(contextKey).(*progressWriter)
	if !ok {
		return &noOpWriter{}, false, ctx
	}
	pw = newWriter(pw, name)
	ctx = context.WithValue(ctx, contextKey, pw)
	return pw, false, ctx
}

func NewContext(ctx context.Context) (ProgressReader, context.Context, func()) {
	pr, pw, cancel := pipe()
	ctx = context.WithValue(ctx, contextKey, pw)
	return pr, ctx, cancel
}

type ProgressWriter interface {
	Write(Progress) error
	Done() error
}

type ProgressReader interface {
	Read(context.Context) (*Progress, error)
}

type Progress struct {
	ID string

	// Progress contains a Message or...
	Message string

	// ...progress of an action
	Action    string
	Current   int
	Total     int
	Timestamp time.Time
	Done      bool
}

type progressReader struct {
	ctx     context.Context
	cond    *sync.Cond
	mu      sync.Mutex
	handles []*streamHandle
}

type streamHandle struct {
	pw    *progressWriter
	lastP *Progress
}

func (sh *streamHandle) next() (*Progress, bool) {
	lasti := sh.pw.lastP.Load()
	if lasti != nil {
		last := lasti.(*Progress)
		if last != sh.lastP {
			sh.lastP = last
			return last, true
		}
	}
	return nil, false
}

func (pr *progressReader) Read(ctx context.Context) (*Progress, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			pr.cond.Broadcast()
		}
	}()
	pr.mu.Lock()
	for {
		select {
		case <-ctx.Done():
			pr.mu.Unlock()
			return nil, ctx.Err()
		default:
		}
		open := false
		for _, sh := range pr.handles { // could be more efficient but unlikely that this array will be very big, maybe random ordering?
			p, ok := sh.next()
			if ok {
				pr.mu.Unlock()
				return p, nil
			}
			if sh.lastP == nil || !sh.lastP.Done {
				open = true
			}
		}
		select {
		case <-pr.ctx.Done():
			if !open {
				pr.mu.Unlock()
				return nil, nil
			}
			pr.cond.Wait()
		default:
			pr.cond.Wait()
		}
	}
}

func (pr *progressReader) append(pw *progressWriter) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	select {
	case <-pr.ctx.Done():
		return
	default:
		pr.handles = append(pr.handles, &streamHandle{pw: pw})
	}
}

func pipe() (*progressReader, *progressWriter, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	pr := &progressReader{
		ctx: ctx,
	}
	pr.cond = sync.NewCond(&pr.mu)
	go func() {
		<-ctx.Done()
		pr.cond.Broadcast()
	}()
	pw := &progressWriter{
		reader: pr,
	}
	return pr, pw, cancel
}

func newWriter(pw *progressWriter, name string) *progressWriter {
	if pw.id != "" {
		name = pw.id + "." + name
	}
	pw = &progressWriter{
		id:     name,
		reader: pw.reader,
	}
	pw.reader.append(pw)
	return pw
}

type progressWriter struct {
	id     string
	lastP  atomic.Value
	done   bool
	reader *progressReader
}

func (pw *progressWriter) Write(p Progress) error {
	if pw.done {
		return errors.Errorf("writing to closed progresswriter %s", pw.id)
	}
	p.ID = pw.id
	if p.Timestamp.IsZero() {
		p.Timestamp = time.Now()
	}
	pw.lastP.Store(&p)
	if p.Done {
		pw.done = true
	}
	pw.reader.cond.Broadcast()
	return nil
}

func (pw *progressWriter) Done() error {
	var p Progress
	lastP := pw.lastP.Load().(*Progress)
	if lastP != nil {
		p = *lastP
		if p.Done {
			return nil
		}
	} else {
		p = Progress{}
	}
	p.Done = true
	return pw.Write(p)
}

type noOpWriter struct{}

func (pw *noOpWriter) Write(p Progress) error {
	return nil
}

func (pw *noOpWriter) Done() error {
	return nil
}

// type ProgressRecord struct {
// 	UUID   string
// 	Parent string
// 	Done   bool
// 	*Progress
// }
