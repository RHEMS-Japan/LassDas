package chain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// FileStore is a local checkpoint, not a model-output validator. The lock
// prevents two runners from acting on the same request concurrently.
type FileStore struct {
	Dir  string
	lock *os.File
}

func Open(dir, request string) (*FileStore, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another process owns this role chain")
	}
	store := &FileStore{Dir: dir, lock: lock}
	state, err := store.Load()
	if errors.Is(err, os.ErrNotExist) {
		err = store.Save(State{Request: request, History: []Result{}})
	} else if err == nil && request != state.Request {
		err = errors.New("the run directory belongs to a different request")
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *FileStore) Close() error { return s.lock.Close() }

func (s *FileStore) Load() (State, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "history.json"))
	if err != nil {
		return State{}, err
	}
	var state State
	err = json.Unmarshal(data, &state)
	return state, err
}

func (s *FileStore) Save(state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.Dir, ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), filepath.Join(s.Dir, "history.json")); err != nil {
		return err
	}
	directory, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
