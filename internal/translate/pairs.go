package translate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Pair struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	DiskBytes int64  `json:"disk_bytes"`
}

var pairFiles = []string{"model.bin", "shared_vocabulary.json", "source.spm", "target.spm"}

func parsePairID(name string) (string, string, bool) {
	if strings.Count(name, "_") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(name, "_", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (m *Manager) Pairs() ([]Pair, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Pair
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		from, to, ok := parsePairID(e.Name())
		if !ok {
			continue
		}
		dir := filepath.Join(m.Dir, e.Name())
		var total int64
		valid := true
		for _, fn := range pairFiles {
			fi, err := os.Stat(filepath.Join(dir, fn))
			if err != nil || !fi.Mode().IsRegular() {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if fi, err := f.Info(); err == nil && fi.Mode().IsRegular() {
				total += fi.Size()
			}
		}
		out = append(out, Pair{ID: e.Name(), From: from, To: to, DiskBytes: total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Manager) findPair(id string) (Pair, bool) {
	pairs, err := m.Pairs()
	if err != nil {
		return Pair{}, false
	}
	for _, p := range pairs {
		if p.ID == id {
			return p, true
		}
	}
	return Pair{}, false
}

func rsLang(lang string) string {
	if lang == "zh" {
		return "zh-Hans"
	}
	return lang
}

func PairIDFor(from, to string) string {
	return rsLang(from) + "_" + rsLang(to)
}

func (m *Manager) EngineFor(from, to string) (Engine, error) {
	if p, ok := m.findPair(PairIDFor(from, to)); ok {
		return &localEngine{mgr: m, pair: p}, nil
	}
	if from != "en" && to != "en" {
		leg1, ok1 := m.findPair(PairIDFor(from, "en"))
		leg2, ok2 := m.findPair(PairIDFor("en", to))
		if ok1 && ok2 {
			return &pivotEngine{
				first:  &localEngine{mgr: m, pair: leg1},
				second: &localEngine{mgr: m, pair: leg2},
			}, nil
		}
	}
	return nil, fmt.Errorf("local model %s not installed", PairIDFor(from, to))
}

type localEngine struct {
	mgr  *Manager
	pair Pair
}

func (e *localEngine) Key() string {
	return "ct2:" + e.pair.ID
}

func (e *localEngine) Translate(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error) {
	texts := make([]string, len(segs))
	for i, s := range segs {
		texts[i] = s.Text
	}
	out, err := e.mgr.getWorker().Translate(ctx, filepath.Join(e.mgr.Dir, e.pair.ID), texts)
	if err != nil {
		return nil, fmt.Errorf("local model %s: %w", e.pair.ID, err)
	}
	if len(out) != len(segs) {
		return nil, fmt.Errorf("local model %s: worker returned %d segments, want %d", e.pair.ID, len(out), len(segs))
	}
	res := make([]Segment, len(segs))
	for i, s := range segs {
		res[i] = Segment{ID: s.ID, Text: out[i]}
	}
	return res, nil
}

type pivotEngine struct {
	first  Engine
	second Engine
}

func (e *pivotEngine) Key() string {
	return e.first.Key() + ">" + e.second.Key()
}

func (e *pivotEngine) Translate(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error) {
	mid, err := e.first.Translate(ctx, segs, src, "en")
	if err != nil {
		return nil, err
	}
	return e.second.Translate(ctx, mid, "en", tgt)
}

func EngineKeyFor(m *Manager, from, to string) (string, error) {
	eng, err := m.EngineFor(from, to)
	if err != nil {
		return "", err
	}
	return eng.Key(), nil
}
