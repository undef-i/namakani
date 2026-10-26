package translate

import (
	"context"
	"errors"
	"regexp"
)

type Segment struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}

type Engine interface {
	Key() string
	Translate(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error)
}

const (
	MaxSegments  = 400
	maxChars     = 80000
	batchSegs    = 25
	batchChars   = 4000
)

var ErrAllFailed = errors.New("translation failed for every segment")

type Service struct {
	Engine Engine
}

func NewService(e Engine) *Service {
	return &Service{Engine: e}
}

var placeholderRe = regexp.MustCompile(`\{/?\d+/?\}`)

func placeholderCounts(s string) map[string]int {
	out := map[string]int{}
	for _, tok := range placeholderRe.FindAllString(s, -1) {
		out[tok]++
	}
	return out
}

func placeholdersMatch(original, translated string) bool {
	a, b := placeholderCounts(original), placeholderCounts(translated)
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (s *Service) TranslateSegments(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error) {
	out := make([]Segment, len(segs))
	copy(out, segs)
	if len(segs) == 0 {
		return out, nil
	}
	index := make(map[int]int, len(segs))
	for i, sg := range segs {
		index[sg.ID] = i
	}
	translated := 0
	hadErr := false
	for i := 0; i < len(segs); {
		j := i
		chars := 0
		for j < len(segs) && j-i < batchSegs {
			chars += len(segs[j].Text)
			if j > i && chars > batchChars {
				break
			}
			j++
		}
		batch := segs[i:j]
		var bad []Segment
		res, err := s.Engine.Translate(ctx, batch, src, tgt)
		if err != nil {
			hadErr = true
			bad = batch
		} else {
			got := make(map[int]string, len(res))
			for _, r := range res {
				got[r.ID] = r.Text
			}
			for _, b := range batch {
				if txt, ok := got[b.ID]; ok && placeholdersMatch(b.Text, txt) {
					out[index[b.ID]].Text = txt
					translated++
				} else {
					bad = append(bad, b)
				}
			}
		}
		for _, b := range bad {
			one, err := s.Engine.Translate(ctx, []Segment{b}, src, tgt)
			if err == nil && len(one) == 1 && one[0].ID == b.ID && placeholdersMatch(b.Text, one[0].Text) {
				out[index[b.ID]].Text = one[0].Text
				translated++
			} else {
				hadErr = true
			}
		}
		i = j
	}
	if translated == 0 && hadErr {
		return nil, ErrAllFailed
	}
	return out, nil
}
