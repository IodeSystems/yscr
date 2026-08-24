// reports.go — HTTP surface for the persisted report artifacts
// (~/.yscr/reports): list + fetch, so a write_report artifact is openable in
// the PWA rather than only a path in chat.
package service

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/yscr/reports"
)

type reportMeta struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// handleReports lists the persisted reports, newest first.
func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	d, err := reports.Dir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	entries, err := os.ReadDir(d)
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]reportMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, reportMeta{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

// handleReport serves one report's markdown by base name (no traversal).
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !strings.HasSuffix(name, ".md") || strings.ContainsAny(name, "/\\") || name == ".." {
		http.Error(w, "bad report name", http.StatusBadRequest)
		return
	}
	d, err := reports.Dir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	b, err := os.ReadFile(filepath.Join(d, name))
	if err != nil {
		http.Error(w, "no such report", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(b)
}
