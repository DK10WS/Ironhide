// Package test holds ironhide's tests.
//
// They live here rather than beside each package because they are written
// against exported APIs and read as one specification of what the system
// accepts and rejects. The consequence is that unexported helpers such as
// upload.hasIndexHTML and api.slugFromHost cannot be called directly, so
// they are covered through their observable behaviour instead: an
// index-detection bug shows up as an upload that is wrongly accepted or
// rejected, and a host-matching bug shows up as a request served from the
// wrong plane.
package test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

// fakeStore implements upload.ObjectStore in memory.
//
// Nothing in the upload path needs Garage: every test here is about what the
// planner accepts, rejects, and names, none of which involves a network.
type fakeStore struct {
	mu   sync.Mutex
	puts []fakePut
	// failAt makes the nth PutObject fail, to exercise partial-failure paths.
	failAt int
}

type fakePut struct {
	Key  string
	Body string
	Size int64
}

func (f *fakeStore) PutObject(_ context.Context, key string, body io.Reader, size int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failAt > 0 && len(f.puts)+1 == f.failAt {
		return "", errors.New("injected store failure")
	}

	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	// A size that disagrees with the body would silently corrupt an object.
	if int64(len(b)) != size {
		return "", errors.New("declared size did not match body length")
	}

	f.puts = append(f.puts, fakePut{Key: key, Body: string(b), Size: size})
	return "etag-" + key, nil
}

func (f *fakeStore) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.puts))
	for _, p := range f.puts {
		out = append(out, p.Key)
	}
	return out
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// reader returns a fresh reader over a stored body.
func (f *fakeStore) reader(key string) io.Reader {
	f.mu.Lock()
	defer f.mu.Unlock()

	// The last write for a key wins, which is what object storage does.
	// Returning the first would make every overwrite invisible: a redeploy
	// would appear to succeed while the site kept serving the old bytes, and
	// no test that redeploys could ever see its result.
	var body string
	found := false
	for _, p := range f.puts {
		if p.Key == key {
			body, found = p.Body, true
		}
	}
	if !found {
		return nil
	}
	return bytes.NewReader([]byte(body))
}
