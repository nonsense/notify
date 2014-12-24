// +build darwin,!kqueue
// +build !fsnotify

package notify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var (
	errAlreadyWatched = errors.New("path is already watched")
	errNotWatched     = errors.New("path is not being watched")
)

var errDepth = errors.New("exceeded allowed iteration count (circular symlink?)")

func canonical(p string) (string, error) {
	for i, depth := 1, 1; i < len(p); i, depth = i+1, depth+1 {
		if depth > 128 {
			return "", &os.PathError{Op: "canonical", Path: p, Err: errDepth}
		}
		if j := strings.IndexRune(p[i:], '/'); j == -1 {
			i = len(p)
		} else {
			i = i + j
		}
		fi, err := os.Lstat(p[:i])
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
			s, err := os.Readlink(p[:i])
			if err != nil {
				return "", err
			}
			p = "/" + s + p[i:]
			i = 1 // no guarantee s is canonical, start all over
		}
	}
	return filepath.Clean(p), nil
}

type watch struct {
	c      chan<- EventInfo
	stream *Stream
	path   string
	events uint32
	isrec  int32
}

func (w *watch) Dispatch(ev []FSEvent) {
	events := atomic.LoadUint32(&w.events)
	isrec := (atomic.LoadInt32(&w.isrec) == 1)
	for i := range ev {
		e := Event(ev[i].Flags & events)
		if e == 0 {
			continue
		}
		if !strings.HasPrefix(ev[i].Path, w.path) {
			continue
		}
		if n := len(w.path); len(ev[i].Path) > n {
			if ev[i].Path[n] != '/' {
				continue
			}
			if !isrec && strings.IndexByte(ev[i].Path[n+1:], '/') != -1 {
				continue
			}
		}
		w.c <- &event{
			fse:   ev[i],
			event: e,
			isdir: ev[i].Flags&FSEventsIsDir != 0,
		}
	}
}

type fsevents struct {
	watches map[string]*watch
	c       chan<- EventInfo
}

func newWatcher() Watcher {
	return &fsevents{
		watches: make(map[string]*watch),
	}
}

func (fse *fsevents) watch(path string, event Event, isrec int32) (err error) {
	if path, err = canonical(path); err != nil {
		return
	}
	if _, ok := fse.watches[path]; ok {
		return errAlreadyWatched
	}
	w := &watch{
		c:      fse.c,
		path:   path,
		events: uint32(event),
		isrec:  isrec,
	}
	w.stream = NewStream(path, w.Dispatch)
	if err = w.stream.Start(); err != nil {
		return
	}
	fse.watches[path] = w
	return nil
}

func (fse *fsevents) unwatch(path string) (err error) {
	if path, err = canonical(path); err != nil {
		return
	}
	w, ok := fse.watches[path]
	if !ok {
		return errNotWatched
	}
	w.stream.Stop()
	delete(fse.watches, path)
	return nil
}

func (fse *fsevents) Watch(path string, event Event) error {
	return fse.watch(path, event, 0)
}

func (fse *fsevents) Unwatch(path string) error {
	return fse.unwatch(path)
}

func (fse *fsevents) Rewatch(path string, oldevent, newevent Event) error {
	w, ok := fse.watches[path]
	if !ok {
		return errNotWatched
	}
	if !atomic.CompareAndSwapUint32(&w.events, uint32(oldevent), uint32(newevent)) {
		return errors.New("invalid event state diff")
	}
	return nil
}

// TODO(rjeczalik): remove
func (fse *fsevents) Dispatch(c chan<- EventInfo, stop <-chan struct{}) {
	fse.c = c
	go func() {
		<-stop
		fse.Stop()
	}()
}

func (fse *fsevents) RecursiveWatch(path string, event Event) error {
	return fse.watch(path, event, 1)
}

func (fse *fsevents) RecursiveUnwatch(path string) error {
	// TODO(rjeczalik): fail if w.isrec == 0?
	return fse.unwatch(path)
}

func (fse *fsevents) RecursiveRewatch(oldpath, newpath string, oldevent, newevent Event) error {
	switch [2]bool{oldpath == newpath, oldevent == newevent} {
	case [2]bool{true, true}:
		w, ok := fse.watches[oldpath]
		if !ok {
			return errNotWatched
		}
		atomic.CompareAndSwapInt32(&w.isrec, 0, 1)
		return nil
	case [2]bool{true, false}:
		w, ok := fse.watches[oldpath]
		if !ok {
			return errNotWatched
		}
		if !atomic.CompareAndSwapUint32(&w.events, uint32(oldevent), uint32(newevent)) {
			return errors.New("invalid event state diff")
		}
		atomic.CompareAndSwapInt32(&w.isrec, 0, 1)
		return nil
	default:
		// TODO(rjeczalik): rewatch newpath only if exists?
		if _, ok := fse.watches[newpath]; ok {
			return errAlreadyWatched
		}
		if err := fse.Unwatch(oldpath); err != nil {
			return err
		}
		// TODO(rjeczalik): revert unwatch if watch fails?
		return fse.watch(newpath, newevent, 1)
	}
}

func (fse *fsevents) Stop() {
	for _, w := range fse.watches {
		w.stream.Stop()
	}
}
