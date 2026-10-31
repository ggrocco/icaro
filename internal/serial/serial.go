// Package serial runs functions one at a time on a goroutine of their
// own. A fake that only touches its state inside Do needs no lock.
package serial

// Queue hands functions to its goroutine.
type Queue chan func()

// New starts a queue's goroutine. It lives as long as the process, which
// suits the test fakes that use it.
func New() Queue {
	q := make(Queue)
	go func() {
		for fn := range q {
			fn()
		}
	}()
	return q
}

// Do runs fn on the queue's goroutine and waits for it to return.
func (q Queue) Do(fn func()) {
	done := make(chan struct{})
	q <- func() {
		defer close(done)
		fn()
	}
	<-done
}
