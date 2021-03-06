package stream

import (
	"sort"
	"sync"

	"github.com/google/uuid"

	"github.com/koss-null/lambda/internal/tools"
)

const (
	u0 = uint(0)
)

type task[T any] struct {
	op Operation[T]
	dt []T
	bm *tools.Bitmask[T]
	wg *sync.WaitGroup
}

type (
	stream[T any] struct {
		onceRun sync.Once

		tasks     chan task[T]
		wrksCnt   uint
		stopWrkrs chan struct{}

		// syncMx is used for Op.sync == true
		syncMx sync.Mutex
		done   map[uuid.UUID]struct{}
		doneMx sync.Mutex

		ops    []Operation[T]
		opsIdx int
		fns    []func([]T, int)

		dt   []T
		dtMu sync.Mutex

		bm   *tools.Bitmask[T]
		mbMx sync.Mutex
	}

	// StreamI describes all functions available for the stream API
	StreamI[T any] interface {
		// Trimming the sequence
		// Skip removes first n elements from underlying slice
		Skip(n uint) StreamI[T]
		// Trim removes last n elements from underlying slice
		Trim(n uint) StreamI[T]

		// Actions on sequence
		// Map executes function on each element of an underlying slice and
		// changes the element to the result of the function
		Map(func(T) T) StreamI[T]
		// Reduce takes the result of the prev operation and applies it to the next item
		Reduce(func(T, T) T) StreamI[T]
		// Filteer
		Filter(func(T) bool) StreamI[T]
		// Sorted sotrs the underlying array
		Sorted(less func(a, b T) bool) StreamI[T]
		// Split splits initial slice into multiple
		Split(func(a T) []T) StreamI[T]

		// Config functions
		// Go splits execution into n goroutines, if multiple Go functions are along
		// the stream pipeline, it applies as soon as it's met in the sequence
		Go(n uint) StreamI[T]

		// Final functions (which does not return the stream)
		// Slice returns resulting slice
		Slice() []T
		// Any returns true if the underlying slice is not empty and at least one element is true
		Any() bool
		// None is not Any()
		None() bool
		// Count returns the length of the result array
		Count() int
		// Sum sums up the items in the array
		Sum(func(int64, T) int64) int64
		// Contains returns true if element is in the array
		Contains(item T, eq func(T, T) bool) bool
	}
)

// Stream creates new instance of stream
func Stream[T any](data []T) StreamI[T] {
	dtCopy := make([]T, len(data))
	copy(dtCopy, data)
	bm := tools.Bitmask[T]{}
	bm.PutLine(0, uint(len(data)), true)
	return &stream[T]{wrksCnt: 1, dt: dtCopy, bm: &bm}
}

// S is a shortened Stream
func S[T any](data []T) StreamI[T] {
	return Stream(data)
}

func (st *stream[T]) Skip(n uint) StreamI[T] {
	st.fns = append(st.fns, func(dt []T, offset int) {
		if offset != 0 {
			return
		}

		st.mbMx.Lock()
		_ = st.bm.CaSBorder(0, true, n)
		st.mbMx.Unlock()

	})
	return st
}

func (st *stream[T]) Trim(n uint) StreamI[T] {
	st.fns = append(st.fns, func(dt []T, offset int) {
		if offset != 0 {
			return
		}

		st.mbMx.Lock()
		_ = st.bm.CaSBorderBw(n)
		st.mbMx.Unlock()

	})
	return st
}

func (st *stream[T]) Map(fn func(T) T) StreamI[T] {
	st.ops = append(st.ops, Operation[T]{
		id:   uuid.New(),
		sync: false,
		fn: func(dt []T, bm *tools.Bitmask[T]) {
			for i := range dt {
				if bm.Get(uint(i)) {
					dt[i] = fn(dt[i])
				}
			}
		},
	})
	return st
}

func (st *stream[T]) Reduce(fn func(T, T) T) StreamI[T] {
	st.fns = append(st.fns, func(dt []T, offset int) {
		if offset == 0 {
			res := dt[0]
			for i := range dt[1:] {
				res = fn(res, dt[i])
			}
			return
		}

	})
	return st
}

func (st *stream[T]) Filter(fn func(T) bool) StreamI[T] {
	st.fns = append(st.fns, func(dt []T, offset int) {

		bm := st.bm.Copy(uint(offset), uint(offset+len(dt)))
		res := make([]bool, len(dt))
		for i := range dt {
			if bm.Get(uint(offset + i)) {
				res[i] = fn(dt[i])
			}
		}

		st.mbMx.Lock()
		for i := range res {
			// Filter does not add already removed items
			if !res[i] && bm.Get(uint(offset+i)) {
				st.bm.Put(uint(offset+i), false)
			}
		}
		st.mbMx.Unlock()

	})
	return st
}

// Sorted adds two functions: first one sorts everything
// FIXME: Sorted does not apply the bitmask
func (st *stream[T]) Sorted(less func(a, b T) bool) StreamI[T] {
	st.fns = append(st.fns, func(dt []T, offset int) {

		sort.Slice(dt, func(i, j int) bool { return less(dt[j], dt[j]) })

	})
	st.fns = append(st.fns, func(dt []T, offset int) {
		if offset != 0 {
			return
		}

		sort.Slice(st.dt, func(i, j int) bool {
			return less(dt[j], dt[j])
		})

	})

	return st
}

func (st *stream[T]) Go(n uint) StreamI[T] {
	return st
}

func (st *stream[T]) Split(sp func(T) []T) StreamI[T] {
	// TODO: implement
	return st
}

func (st *stream[T]) nextOp() Operation[T] {
	st.opsIdx++
	return st.ops[st.opsIdx-1]
}

// FIXME: don't need this
func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func (st *stream[T]) addTasks() {
	st.dt = st.bm.Apply(st.dt)
	st.bm.PutLine(0, uint(len(st.dt)), true)

	dataLen := uint64(len(st.dt))
	blockSize := min(minSplitLen, dataLen)
	if dataLen/uint64(st.wrksCnt) > blockSize {
		blockSize = dataLen / uint64(st.wrksCnt)
	}

	lf, rg := uint64(0), blockSize
	op := st.nextOp()
	for uint64(lf) < dataLen {
		st.tasks <- task[T]{
			op: op,
			dt: st.dt[lf:rg],
			bm: st.bm.ShallowCopy(uint(lf), uint(rg)),
			wg: &sync.WaitGroup{},
		}
		lf = rg
		rg = min(rg+blockSize, dataLen)
	}

	return
}

// run start executing st.fns
func (st *stream[T]) run() {
	st.onceRun.Do(func() {
		st.addTasks()
		st.startWorkers()
	})
}

func (st *stream[T]) startWorkers() {
	done := make(chan struct{})
	st.stopWrkrs = done
	for i := 0; uint(i) < st.wrksCnt; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				case t := <-st.tasks:
					if t.op.Sync() {
						st.syncMx.Lock()
						t.wg.Wait()

						st.doneMx.Lock()
						if _, ok := st.done[t.op.id]; !ok {
							continue
						}
						st.done[t.op.id] = struct{}{}
						st.doneMx.Unlock()

						t.op.Do(t.dt, t.bm)
						st.addTasks()

						st.syncMx.Unlock()
						continue
					}

					t.op.Do(t.dt, t.bm)
					t.wg.Done()
					no := st.nextOp()
					if !no.sync {
						t.wg.Add(1)
					}
					st.tasks <- task[T]{no, t.dt, t.bm, t.wg}
				}
			}
		}()
	}
}

func (st *stream[T]) Slice() []T {
	var data []T
	st.fns = append(st.fns, func(dt []T, offset int) {
		if offset != 0 {
			return
		}
		for i := u0; i < st.wrksCnt; i++ {
		}

		st.dtMu.Lock()
		st.mbMx.Lock()
		defer st.dtMu.Unlock()
		defer st.mbMx.Unlock()

		data = make([]T, 0, len(st.dt))
		for _, dt := range st.dt {
			_, exist := st.bm.Next()
			if exist {
				data = append(data, dt)
			}
		}
	})

	st.run()
	return data
}

func (st *stream[T]) Any() bool {
	return !st.None()
}

func (st *stream[T]) None() bool {
	return st.Count() == 0
}

func (st *stream[T]) Count() int {
	st.run()

	st.mbMx.Lock()
	defer st.mbMx.Unlock()
	return int(st.bm.CountOnes())
}

func (st *stream[T]) Sum(sum func(int64, T) int64) int64 {
	if sum == nil {
		return 0
	}

	var s int64
	st.fns = append(st.fns, func(a []T, offset int) {
		if offset != 0 {
			return
		}
		for _, dt := range st.bm.Apply(st.dt) {
			s = sum(s, dt)
		}
	})

	st.run()
	return s
}

// Contains returns if the underlying array contains an item
func (st *stream[T]) Contains(item T, eq func(a, b T) bool) bool {
	for i := range st.dt {
		if eq(st.dt[i], item) {
			return true
		}
	}
	return false
}
