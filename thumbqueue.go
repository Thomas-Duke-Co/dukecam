package main

import (
	"log"
	"sync"
)

// ─── Thumbnail Queue ─────────────────────────────────────────────

// Thumbnail rendering is the expensive half of an upload: decoding a 24 MP
// phone JPEG and running a Lanczos fit costs roughly 120ms per megapixel, and
// it used to happen inline on the request goroutine while the client waited.
// Rendering it here instead takes that cost off the request path.
//
// ServeThumb already falls back to the full-size photo when a thumbnail is
// missing, so a photo stays viewable during the brief window before its
// thumbnail lands — no placeholder handling is needed.

type thumbJob struct {
	data    []byte
	path    string
	quality int
}

// ThumbQueue renders thumbnails on a small, bounded worker pool.
type ThumbQueue struct {
	jobs chan thumbJob
	wg   sync.WaitGroup
}

// NewThumbQueue starts workers goroutines draining a buffered queue.
// Keep workers well below the core count — this box has 4 physical cores and
// 59 containers competing for them.
func NewThumbQueue(workers, depth int) *ThumbQueue {
	if workers < 1 {
		workers = 1
	}
	if depth < 1 {
		depth = 1
	}
	q := &ThumbQueue{jobs: make(chan thumbJob, depth)}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	log.Printf("thumbnail queue started: workers=%d depth=%d", workers, depth)
	return q
}

func (q *ThumbQueue) worker() {
	defer q.wg.Done()
	for j := range q.jobs {
		render(j)
	}
}

func render(j thumbJob) {
	if err := MakeThumbFromBytes(j.data, j.path, j.quality); err != nil {
		log.Printf("thumb render error: path=%s err=%v", j.path, err)
	}
}

// Enqueue schedules a thumbnail render. If the queue is saturated the render
// runs inline rather than being dropped — that degrades to the old synchronous
// behaviour under sustained load instead of silently losing a thumbnail.
func (q *ThumbQueue) Enqueue(data []byte, path string, quality int) {
	if path == "" {
		return
	}
	j := thumbJob{data: data, path: path, quality: quality}
	select {
	case q.jobs <- j:
	default:
		log.Printf("thumb queue full, rendering inline: path=%s", path)
		render(j)
	}
}

// Shutdown drains outstanding renders. Call on graceful shutdown so a photo
// uploaded moments before exit still gets its thumbnail.
func (q *ThumbQueue) Shutdown() {
	close(q.jobs)
	q.wg.Wait()
}
