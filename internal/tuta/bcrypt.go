package tuta

import "encoding/binary"

type blowfishState struct {
	p [18]uint32
	s [4][256]uint32
}

func newBlowfishState() *blowfishState {
	st := &blowfishState{}
	copy(st.p[:], bfP[:])
	copy(st.s[0][:], bfS0[:])
	copy(st.s[1][:], bfS1[:])
	copy(st.s[2][:], bfS2[:])
	copy(st.s[3][:], bfS3[:])
	return st
}

func (st *blowfishState) f(x uint32) uint32 {
	return ((st.s[0][x>>24] + st.s[1][(x>>16)&0xff]) ^ st.s[2][(x>>8)&0xff]) + st.s[3][x&0xff]
}

func (st *blowfishState) encryptPair(l, r uint32) (uint32, uint32) {
	l ^= st.p[0]
	for i := 1; i <= 15; i += 2 {
		r ^= st.f(l) ^ st.p[i]
		l ^= st.f(r) ^ st.p[i+1]
	}
	r ^= st.p[17]
	return r, l
}

func streamWord(data []byte, off *int) uint32 {
	var w uint32
	for i := 0; i < 4; i++ {
		w = w<<8 | uint32(data[*off])
		*off = (*off + 1) % len(data)
	}
	return w
}

func (st *blowfishState) expandKeyWithSalt(key, salt []byte) {
	koff := 0
	for i := range st.p {
		st.p[i] ^= streamWord(key, &koff)
	}
	var l, r uint32
	soff := 0
	for i := 0; i < 18; i += 2 {
		l ^= streamWord(salt, &soff)
		r ^= streamWord(salt, &soff)
		l, r = st.encryptPair(l, r)
		st.p[i] = l
		st.p[i+1] = r
	}
	for b := 0; b < 4; b++ {
		for i := 0; i < 256; i += 2 {
			l ^= streamWord(salt, &soff)
			r ^= streamWord(salt, &soff)
			l, r = st.encryptPair(l, r)
			st.s[b][i] = l
			st.s[b][i+1] = r
		}
	}
}

func (st *blowfishState) expandKey(key []byte) {
	koff := 0
	for i := range st.p {
		st.p[i] ^= streamWord(key, &koff)
	}
	var l, r uint32
	for i := 0; i < 18; i += 2 {
		l, r = st.encryptPair(l, r)
		st.p[i] = l
		st.p[i+1] = r
	}
	for b := 0; b < 4; b++ {
		for i := 0; i < 256; i += 2 {
			l, r = st.encryptPair(l, r)
			st.s[b][i] = l
			st.s[b][i+1] = r
		}
	}
}

func bcryptCryptRaw(password, salt []byte, logRounds uint) []byte {
	st := newBlowfishState()
	st.expandKeyWithSalt(password, salt)
	rounds := 1 << logRounds
	for i := 0; i < rounds; i++ {
		st.expandKey(password)
		st.expandKey(salt)
	}
	data := make([]byte, len(bcryptMagic))
	copy(data, bcryptMagic)
	for i := 0; i < 64; i++ {
		for j := 0; j < len(data); j += 8 {
			l := binary.BigEndian.Uint32(data[j:])
			r := binary.BigEndian.Uint32(data[j+4:])
			l, r = st.encryptPair(l, r)
			binary.BigEndian.PutUint32(data[j:], l)
			binary.BigEndian.PutUint32(data[j+4:], r)
		}
	}
	return data
}
