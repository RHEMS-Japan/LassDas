package chain

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// liveFiles is the running copy of one process's output. It exists only while
// the process runs; the completed record in the history is the durable form.
type liveFiles struct {
	base   string
	stdout *liveWriter
	stderr *liveWriter
}

// openLive prepares the live copy under the process's Live directory, or does
// nothing when none is configured. A failure to prepare it never fails the
// process: it returns a note for the record instead, so the run goes on and
// the operator can see why nothing was shown.
func openLive(p Process, role Role, assignment Assignment, env map[string]string, secrets []string) (*liveFiles, string) {
	if p.Live == "" {
		return nil, ""
	}
	if err := os.MkdirAll(p.Live, 0700); err != nil {
		return nil, "live output was not written: " + err.Error()
	}
	name := strings.NewReplacer("/", "_", string(filepath.Separator), "_", "..", "_").Replace(role.Name + "-" + p.Name)
	base := filepath.Join(p.Live, name)
	record := map[string]any{"role": role.Name, "speaker": p.Name, "started_at": time.Now().UTC(),
		"instruction": assignment.Instruction, "home": env["TASK_HOME"], "workspace": env["TASK_WORKSPACE"]}
	if p.ModelEnv != "" {
		record["model"] = env[p.ModelEnv]
	}
	data, _ := json.Marshal(record)
	files := &liveFiles{base: base}
	for _, step := range []struct {
		suffix string
		target **liveWriter
	}{{".stdout", &files.stdout}, {".stderr", &files.stderr}} {
		file, err := os.OpenFile(base+step.suffix, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			files.close()
			return nil, "live output was not written: " + err.Error()
		}
		*step.target = newLiveWriter(file, secrets)
	}
	if err := os.WriteFile(base+".json", data, 0600); err != nil {
		files.close()
		return nil, "live output was not written: " + err.Error()
	}
	return files, ""
}

// close flushes what was held back and removes the live copy: the history
// record that follows carries the complete, scrubbed text.
func (f *liveFiles) close() {
	if f == nil {
		return
	}
	for _, writer := range []*liveWriter{f.stdout, f.stderr} {
		if writer != nil {
			writer.Close()
		}
	}
	for _, suffix := range []string{".json", ".stdout", ".stderr"} {
		os.Remove(f.base + suffix)
	}
}

// liveWriter copies output to a file as it arrives with every configured
// credential replaced before it reaches the disk. The last len(longest
// credential)-1 bytes are held back until more arrive or the process ends, so
// a credential split across two writes is still replaced. It never reports an
// error to the process: a broken live copy must not interrupt the role.
type liveWriter struct {
	file    *os.File
	secrets []string
	hold    int
	tail    []byte
	broken  bool
}

func newLiveWriter(file *os.File, secrets []string) *liveWriter {
	w := &liveWriter{file: file}
	for _, secret := range secrets {
		if secret != "" {
			w.secrets = append(w.secrets, secret)
			if len(secret)-1 > w.hold {
				w.hold = len(secret) - 1
			}
		}
	}
	return w
}

func (w *liveWriter) scrub(data []byte) []byte {
	for _, secret := range w.secrets {
		data = bytes.ReplaceAll(data, []byte(secret), []byte("[credential]"))
	}
	return data
}

func (w *liveWriter) Write(p []byte) (int, error) {
	if w.broken {
		return len(p), nil
	}
	data := w.scrub(append(w.tail, p...))
	keep := w.hold
	if keep > len(data) {
		keep = len(data)
	}
	w.tail = append([]byte(nil), data[len(data)-keep:]...)
	if _, err := w.file.Write(data[:len(data)-keep]); err != nil {
		w.broken = true
	}
	return len(p), nil
}

func (w *liveWriter) Close() error {
	if !w.broken && len(w.tail) > 0 {
		w.file.Write(w.scrub(w.tail))
	}
	w.tail = nil
	return w.file.Close()
}
