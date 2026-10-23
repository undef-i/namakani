package proton

import (
	"context"
	"sync"
)

const (
	turnNone = 0
	turnUser = 1
	turnBack = 2
)

func (s *Session) turnInitLocked() {
	if s.tcond == nil {
		s.tcond = sync.NewCond(&s.tmu)
	}
}

func (s *Session) UserTurn() func() {
	s.tmu.Lock()
	s.turnInitLocked()
	s.tusers++
	for s.tholder != turnNone {
		s.tcond.Wait()
	}
	s.tholder = turnUser
	s.tusers--
	s.tmu.Unlock()
	return func() {
		s.tmu.Lock()
		s.tholder = turnNone
		s.tcond.Broadcast()
		s.tmu.Unlock()
	}
}

func (s *Session) BackgroundTurn(ctx context.Context) (func(), bool) {
	s.tmu.Lock()
	s.turnInitLocked()
	if ctx.Err() != nil {
		s.tmu.Unlock()
		return nil, false
	}
	stop := context.AfterFunc(ctx, func() {
		s.tmu.Lock()
		s.tcond.Broadcast()
		s.tmu.Unlock()
	})
	defer stop()
	for s.tholder != turnNone || s.tusers > 0 {
		if ctx.Err() != nil {
			s.tmu.Unlock()
			return nil, false
		}
		s.tcond.Wait()
	}
	s.tholder = turnBack
	s.tmu.Unlock()
	return func() {
		s.tmu.Lock()
		s.tholder = turnNone
		s.tcond.Broadcast()
		s.tmu.Unlock()
	}, true
}
