// Package qr makes QR codes: byte mode, error correction level L, versions 1 to 20 (up to 858 bytes), the mask chosen by the penalty rules
// of the standard. It exists to check, against an independent reading of the result, the JavaScript encoder the VPN page uses to show a
// device's WireGuard file as a code (web/static/scripts/qr.js is the same algorithm; the tests compare the two byte for byte).
package qr

import "errors"

const (
	MaxVersion = 20
	// level L: error correction codewords per block and number of blocks, by version (index 0 unused)
	maxData = 858
)

var eccPerBlock = [MaxVersion + 1]int{0, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28}
var numBlocks = [MaxVersion + 1]int{0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8}

// ErrTooBig is returned for data that does not fit in version 20.
var ErrTooBig = errors.New("qr: the data is too big for a QR code of this kind")

func size(ver int) int { return ver*4 + 17 }

// RawCodewords is how many codewords (data and error correction) a version holds.
func RawCodewords(ver int) int {
	r := (16*ver+128)*ver + 64
	if ver >= 2 {
		n := ver/7 + 2
		r -= (25*n-10)*n - 55
		if ver >= 7 {
			r -= 36
		}
	}
	return r / 8
}

// DataCodewords is how many of them are data.
func DataCodewords(ver int) int { return RawCodewords(ver) - eccPerBlock[ver]*numBlocks[ver] }

// Capacity is how many bytes a version holds in byte mode.
func Capacity(ver int) int {
	bits := DataCodewords(ver)*8 - 4 - countBits(ver)
	return bits / 8
}

func countBits(ver int) int {
	if ver >= 10 {
		return 16
	}
	return 8
}

// ---- Reed-Solomon over GF(256), polynomial 0x11D -----------------------------------------------------------------

func gfMul(x, y int) int {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= ((y >> uint(i)) & 1) * x
	}
	return z
}

func rsDivisor(degree int) []int {
	res := make([]int, degree)
	res[degree-1] = 1
	root := 1
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			res[j] = gfMul(res[j], root)
			if j+1 < degree {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return res
}

func rsRemainder(data, divisor []int) []int {
	res := make([]int, len(divisor))
	for _, b := range data {
		factor := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i, c := range divisor {
			res[i] ^= gfMul(c, factor)
		}
	}
	return res
}

// ---- the data -------------------------------------------------------------------------------------------------------

func dataCodewords(data []byte, ver int) []int {
	var bits []int
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>uint(i))&1)
		}
	}
	put(4, 4)
	put(len(data), countBits(ver))
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := DataCodewords(ver) * 8
	for t := 0; t < 4 && len(bits) < capBits; t++ {
		bits = append(bits, 0)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	out := make([]int, 0, DataCodewords(ver))
	for i := 0; i < len(bits); i += 8 {
		v := 0
		for j := 0; j < 8; j++ {
			v = v<<1 | bits[i+j]
		}
		out = append(out, v)
	}
	for pad := 0xEC; len(out) < DataCodewords(ver); pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

func interleave(data []int, ver int) []int {
	nb, ecc, raw := numBlocks[ver], eccPerBlock[ver], RawCodewords(ver)
	short := nb - raw%nb
	shortLen := raw / nb
	div := rsDivisor(ecc)
	var blocks [][]int
	k := 0
	for i := 0; i < nb; i++ {
		l := shortLen - ecc
		if i >= short {
			l++
		}
		d := data[k : k+l]
		k += l
		block := append(append([]int{}, d...), rsRemainder(d, div)...)
		if i < short {
			block = append(block[:l:l], append([]int{0}, block[l:]...)...) // a hole where the longer blocks have their extra data byte
		}
		blocks = append(blocks, block)
	}
	var out []int
	for i := 0; i < len(blocks[0]); i++ {
		for j, b := range blocks {
			if i != shortLen-ecc || j >= short {
				out = append(out, b[i])
			}
		}
	}
	return out
}

// ---- the matrix -----------------------------------------------------------------------------------------------------

type matrix struct {
	n   int
	mod [][]bool
	fn  [][]bool
}

func newMatrix(ver int) *matrix {
	n := size(ver)
	m := &matrix{n: n, mod: make([][]bool, n), fn: make([][]bool, n)}
	for i := range m.mod {
		m.mod[i] = make([]bool, n)
		m.fn[i] = make([]bool, n)
	}
	return m
}

func (m *matrix) set(x, y int, dark bool) { m.mod[y][x], m.fn[y][x] = dark, true }

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func (m *matrix) finder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x >= 0 && x < m.n && y >= 0 && y < m.n {
				d := abs(dx)
				if abs(dy) > d {
					d = abs(dy)
				}
				m.set(x, y, d != 2 && d != 4)
			}
		}
	}
}

func (m *matrix) alignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			d := abs(dx)
			if abs(dy) > d {
				d = abs(dy)
			}
			m.set(cx+dx, cy+dy, d != 1)
		}
	}
}

func alignmentPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := (ver*4 + n*2 + 1) / (n*2 - 2) * 2
	res := make([]int, n)
	res[0] = 6
	for i, pos := n-1, size(ver)-7; i >= 1; i, pos = i-1, pos-step {
		res[i] = pos
	}
	return res
}

func bit(v, i int) bool { return (v>>uint(i))&1 != 0 }

func formatWord(mask int) int {
	data := 1<<3 | mask // level L is 01
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

func versionWord(ver int) int {
	rem := ver
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return ver<<12 | rem
}

func (m *matrix) formatBits(mask int) {
	bits := formatWord(mask)
	for i := 0; i <= 5; i++ {
		m.set(8, i, bit(bits, i))
	}
	m.set(8, 7, bit(bits, 6))
	m.set(8, 8, bit(bits, 7))
	m.set(7, 8, bit(bits, 8))
	for i := 9; i < 15; i++ {
		m.set(14-i, 8, bit(bits, i))
	}
	for i := 0; i < 8; i++ {
		m.set(m.n-1-i, 8, bit(bits, i))
	}
	for i := 8; i < 15; i++ {
		m.set(8, m.n-15+i, bit(bits, i))
	}
	m.set(8, m.n-8, true)
}

func (m *matrix) versionBits(ver int) {
	if ver < 7 {
		return
	}
	bits := versionWord(ver)
	for i := 0; i < 18; i++ {
		a, b := m.n-11+i%3, i/3
		m.set(a, b, bit(bits, i))
		m.set(b, a, bit(bits, i))
	}
}

func (m *matrix) functionPatterns(ver int) {
	for i := 0; i < m.n; i++ {
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}
	m.finder(3, 3)
	m.finder(m.n-4, 3)
	m.finder(3, m.n-4)
	pos := alignmentPositions(ver)
	for i, x := range pos {
		for j, y := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			m.alignment(x, y)
		}
	}
	m.formatBits(0) // reserves the places
	m.versionBits(ver)
}

func (m *matrix) place(data []int) {
	i := 0
	for right := m.n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < m.n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = m.n - 1 - vert
				}
				if !m.fn[y][x] && i < len(data)*8 {
					m.mod[y][x] = bit(data[i>>3], 7-(i&7))
					i++
				}
			}
		}
	}
}

func maskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	}
	return ((x+y)%2+x*y%3)%2 == 0
}

func (m *matrix) applyMask(mask int) {
	for y := 0; y < m.n; y++ {
		for x := 0; x < m.n; x++ {
			if !m.fn[y][x] && maskBit(mask, x, y) {
				m.mod[y][x] = !m.mod[y][x]
			}
		}
	}
}

// penalty is the score of the standard's four rules (lower is better).
func (m *matrix) penalty() int {
	n := m.n
	score := 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i < n; i++ {
			if get(i) == get(i-1) {
				run++
				if run == 5 {
					score += 3
				} else if run > 5 {
					score++
				}
			} else {
				run = 1
			}
		}
		// the pattern 1:1:3:1:1 with four light modules on a side
		pat := func(p string) {
			for s := 0; s+len(p) <= n; s++ {
				ok := true
				for k := 0; k < len(p); k++ {
					if get(s+k) != (p[k] == '1') {
						ok = false
						break
					}
				}
				if ok {
					score += 40
				}
			}
		}
		pat("10111010000")
		pat("00001011101")
	}
	for y := 0; y < n; y++ {
		y := y
		line(func(i int) bool { return m.mod[y][i] })
	}
	for x := 0; x < n; x++ {
		x := x
		line(func(i int) bool { return m.mod[i][x] })
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if m.mod[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n && m.mod[y][x] == m.mod[y][x+1] && m.mod[y][x] == m.mod[y+1][x] && m.mod[y][x] == m.mod[y+1][x+1] {
				score += 3
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10) + total - 1) / total
	score += (k - 1) * 10
	return score
}

// Encode makes the code of data: the modules, dark ones true, row by row, with the version used.
func Encode(data []byte) (modules [][]bool, version int, err error) {
	ver := 0
	for v := 1; v <= MaxVersion; v++ {
		if len(data) <= Capacity(v) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, 0, ErrTooBig
	}
	words := interleave(dataCodewords(data, ver), ver)
	best, bestScore := (*matrix)(nil), 0
	for mask := 0; mask < 8; mask++ {
		m := newMatrix(ver)
		m.functionPatterns(ver)
		m.place(words)
		m.applyMask(mask)
		m.formatBits(mask)
		if p := m.penalty(); best == nil || p < bestScore {
			best, bestScore = m, p
		}
	}
	return best.mod, ver, nil
}
