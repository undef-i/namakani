package tuta

import (
	"encoding/binary"
	"errors"
)

func lz4Decompress(input []byte) ([]byte, error) {
	out := make([]byte, 0, len(input)*6)
	i := 0
	for i < len(input) {
		token := input[i]
		i++
		litLen := int(token >> 4)
		if litLen > 0 {
			if litLen == 15 {
				for {
					if i >= len(input) {
						return nil, errors.New("tuta: lz4 literal length overruns input")
					}
					l := int(input[i])
					i++
					litLen += l
					if l != 255 {
						break
					}
				}
			}
			if i+litLen > len(input) {
				return nil, errors.New("tuta: lz4 literals overrun input")
			}
			out = append(out, input[i:i+litLen]...)
			i += litLen
			if i == len(input) {
				break
			}
		}
		if i+2 > len(input) {
			return nil, errors.New("tuta: lz4 offset overruns input")
		}
		offset := int(binary.LittleEndian.Uint16(input[i:]))
		i += 2
		if offset == 0 || offset > len(out) {
			return nil, errors.New("tuta: lz4 invalid offset")
		}
		matchLen := int(token & 0xf)
		if matchLen == 15 {
			for {
				if i >= len(input) {
					return nil, errors.New("tuta: lz4 match length overruns input")
				}
				l := int(input[i])
				i++
				matchLen += l
				if l != 255 {
					break
				}
			}
		}
		matchLen += 4
		pos := len(out) - offset
		for k := 0; k < matchLen; k++ {
			out = append(out, out[pos+k])
		}
	}
	return out, nil
}

func lz4Compress(input []byte) []byte {
	var out []byte
	i := 0
	for i < len(input) {
		start := i
		bestLen, bestOff := 0, 0
		windowStart := i - 65535
		if windowStart < 0 {
			windowStart = 0
		}
		for j := windowStart; j+4 <= i; j++ {
			l := 0
			for i+l < len(input) && input[j+l] == input[i+l] && l < 65535 {
				l++
			}
			if l >= 4 && l > bestLen {
				bestLen, bestOff = l, i-j
			}
		}
		litLen := i - start
		if bestLen == 0 {
			litLen = len(input) - start
			token := byte(0)
			if litLen < 15 {
				token = byte(litLen << 4)
			} else {
				token = 0xf0
			}
			out = append(out, token)
			if litLen >= 15 {
				rem := litLen - 15
				for rem >= 255 {
					out = append(out, 255)
					rem -= 255
				}
				out = append(out, byte(rem))
			}
			out = append(out, input[start:]...)
			break
		}
		matchLen := bestLen - 4
		token := byte(0)
		if litLen < 15 {
			token |= byte(litLen << 4)
		} else {
			token |= 0xf0
		}
		if matchLen < 15 {
			token |= byte(matchLen)
		} else {
			token |= 0x0f
		}
		out = append(out, token)
		if litLen >= 15 {
			rem := litLen - 15
			for rem >= 255 {
				out = append(out, 255)
				rem -= 255
			}
			out = append(out, byte(rem))
		}
		out = append(out, input[start:i]...)
		var offb [2]byte
		binary.LittleEndian.PutUint16(offb[:], uint16(bestOff))
		out = append(out, offb[:]...)
		if matchLen >= 15 {
			rem := matchLen - 15
			for rem >= 255 {
				out = append(out, 255)
				rem -= 255
			}
			out = append(out, byte(rem))
		}
		i += bestLen
		if i >= len(input) {
			break
		}
	}
	return out
}
