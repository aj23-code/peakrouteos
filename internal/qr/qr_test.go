package qr

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// decode reads a code back the way a scanner does, without using the encoder's placement code: the format word (both copies), the
// mask, the zig-zag of the modules, the blocks, the Reed-Solomon check of every block, and the byte mode segment.
func decode(t *testing.T, mod [][]bool) []byte {
	t.Helper()
	n := len(mod)
	ver := (n - 17) / 4
	if size(ver) != n {
		t.Fatalf("a side of %d is no version", n)
	}
	b := func(x, y int) int {
		if mod[y][x] {
			return 1
		}
		return 0
	}
	// format information: the first copy around the top left finder, the second beside the other two
	var f1, f2 int
	pos1 := [][2]int{{8, 0}, {8, 1}, {8, 2}, {8, 3}, {8, 4}, {8, 5}, {8, 7}, {8, 8}, {7, 8}, {5, 8}, {4, 8}, {3, 8}, {2, 8}, {1, 8}, {0, 8}}
	for i, p := range pos1 {
		f1 |= b(p[0], p[1]) << uint(i)
	}
	for i := 0; i < 8; i++ {
		f2 |= b(n-1-i, 8) << uint(i)
	}
	for i := 8; i < 15; i++ {
		f2 |= b(8, n-15+i) << uint(i)
	}
	if f1 != f2 {
		t.Fatalf("the two copies of the format word differ: %015b %015b", f1, f2)
	}
	mask := -1
	for m := 0; m < 8; m++ {
		if formatWord(m) == f1 {
			mask = m
		}
	}
	if mask < 0 {
		t.Fatalf("%015b is no format word of level L", f1)
	}
	if !mod[n-8][8] {
		t.Fatal("the dark module is light")
	}
	// the version information
	if ver >= 7 {
		v := 0
		for i := 0; i < 18; i++ {
			v |= b(n-11+i%3, i/3) << uint(i)
		}
		if v != versionWord(ver) {
			t.Fatalf("version word %018b want %018b", v, versionWord(ver))
		}
	}
	ref := newMatrix(ver)
	ref.functionPatterns(ver)
	// the zig-zag, written again here
	var bits []int
	col := n - 1
	up := true
	for col > 0 {
		if col == 6 {
			col--
		}
		for k := 0; k < n; k++ {
			y := k
			if up {
				y = n - 1 - k
			}
			for _, x := range []int{col, col - 1} {
				if ref.fn[y][x] {
					continue
				}
				v := b(x, y)
				if maskBit(mask, x, y) {
					v ^= 1
				}
				bits = append(bits, v)
			}
		}
		up = !up
		col -= 2
	}
	raw := RawCodewords(ver)
	words := make([]int, raw)
	for i := 0; i < raw; i++ {
		for j := 0; j < 8; j++ {
			words[i] = words[i]<<1 | bits[i*8+j]
		}
	}
	for _, rest := range bits[raw*8:] {
		if rest != 0 {
			t.Fatal("remainder bits are not zero")
		}
	}
	// deinterleave
	nb, ecc := numBlocks[ver], eccPerBlock[ver]
	short := nb - raw%nb
	shortLen := raw / nb
	dl := make([]int, nb)
	blocks := make([][]int, nb)
	for j := range dl {
		dl[j] = shortLen - ecc
		if j >= short {
			dl[j]++
		}
	}
	k := 0
	for i := 0; i < shortLen-ecc+1; i++ { // the data, one codeword from each block in turn
		for j := 0; j < nb; j++ {
			if i < dl[j] {
				blocks[j] = append(blocks[j], words[k])
				k++
			}
		}
	}
	for i := 0; i < ecc; i++ { // then the error correction
		for j := 0; j < nb; j++ {
			blocks[j] = append(blocks[j], words[k])
			k++
		}
	}
	var data []int
	for j, blk := range blocks {
		l := len(blk) - ecc
		if l != dl[j] {
			t.Fatalf("block %d has %d data codewords", j, l)
		}
		// every codeword polynomial must vanish at 2^0 .. 2^(ecc-1)
		root := 1
		for r := 0; r < ecc; r++ {
			acc := 0
			for _, c := range blk {
				acc = gfMul(acc, root) ^ c
			}
			if acc != 0 {
				t.Fatalf("block %d does not check out at root %d", j, r)
			}
			root = gfMul(root, 2)
		}
		data = append(data, blk[:l]...)
	}
	// the byte mode segment
	var dbits []int
	for _, w := range data {
		for j := 7; j >= 0; j-- {
			dbits = append(dbits, (w>>uint(j))&1)
		}
	}
	read := func(from, n int) int {
		v := 0
		for i := 0; i < n; i++ {
			v = v<<1 | dbits[from+i]
		}
		return v
	}
	if read(0, 4) != 4 {
		t.Fatalf("mode %d", read(0, 4))
	}
	cb := countBits(ver)
	count := read(4, cb)
	out := make([]byte, count)
	for i := range out {
		out[i] = byte(read(4+cb+i*8, 8))
	}
	return out
}

func TestTheCodeReadsBackAsWhatWentIn(t *testing.T) {
	for _, n := range []int{0, 1, 17, 18, 19, 20, 32, 33, 34, 100, 134, 135, 136, 150, 230, 271, 272, 330, 331, 400, 500, 600, 700, 800, 858} {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i*7 + n)
		}
		mod, ver, err := Encode(data)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if got := decode(t, mod); !bytes.Equal(got, data) {
			t.Fatalf("%d bytes (version %d) came back as %x", n, ver, got)
		}
	}
	// text like a WireGuard file
	text := "[Interface]\nPrivateKey = 4NejFm9GKo6oO6vLckBVHdGHbs2Sit6tpt72T/kRTmQ=\nAddress = 10.8.0.2/32\nDNS = 10.8.0.1\n\n[Peer]\nPublicKey = xtq7cs4u/xPCxgMJ69a423zFe24EhIgiZRRP93RiCA8=\nAllowedIPs = 192.168.1.0/24, 10.8.0.0/24\nEndpoint = vpn.example.com:51821\nPersistentKeepalive = 25\n"
	mod, ver, err := Encode([]byte(text))
	if err != nil || string(decode(t, mod)) != text {
		t.Fatalf("%v", err)
	}
	t.Logf("a file of %d bytes is version %d (%d modules)", len(text), ver, len(mod))
}

func TestTheNumbersBehindTheCode(t *testing.T) {
	// known: the format word of level L with mask 0, and the version words of 7 and 8
	if formatWord(0) != 0b111011111000100 || formatWord(1) != 0b111001011110011 {
		t.Fatalf("%015b %015b", formatWord(0), formatWord(1))
	}
	if versionWord(7) != 0b000111110010010100 || versionWord(8) != 0b001000010110111100 {
		t.Fatalf("%018b %018b", versionWord(7), versionWord(8))
	}
	// known: capacities of level L in byte mode
	for ver, want := range map[int]int{1: 17, 2: 32, 3: 53, 4: 78, 5: 106, 6: 134, 7: 154, 8: 192, 9: 230, 10: 271, 14: 458, 20: 858} {
		if got := Capacity(ver); got != want {
			t.Errorf("version %d holds %d, want %d", ver, got, want)
		}
	}
	// the codewords add up in every version, and the alignment patterns are where the standard has them
	for v := 1; v <= MaxVersion; v++ {
		if DataCodewords(v) <= 0 || RawCodewords(v) < DataCodewords(v) {
			t.Errorf("version %d", v)
		}
	}
	for ver, want := range map[int][]int{1: nil, 2: {6, 18}, 7: {6, 22, 38}, 14: {6, 26, 46, 66}, 20: {6, 34, 62, 90}} {
		if got := fmt.Sprint(alignmentPositions(ver)); got != fmt.Sprint(want) && !(want == nil && got == "[]") {
			t.Errorf("version %d: %s want %v", ver, got, want)
		}
	}
	// Reed-Solomon: the known 7 code words of "HELLO WORLD" 1-M are not level L, so check the arithmetic itself: x^7 + 127x^6 + 122x^5 + 154x^4 + 164x^3 + 11x^2 + 68x + 117
	if got := fmt.Sprint(rsDivisor(7)); got != "[127 122 154 164 11 68 117]" {
		t.Fatalf("the generator of degree 7: %s", got)
	}
	if gfMul(2, 128) != 29 || gfMul(0x57, 0x83) != gfMul(0x83, 0x57) {
		t.Fatal("the field")
	}
}

func TestTooBigAndTheMaskIsTheBestOfEight(t *testing.T) {
	if _, _, err := Encode(make([]byte, 859)); !errors.Is(err, ErrTooBig) {
		t.Fatalf("%v", err)
	}
	data := []byte(strings.Repeat("peakroute ", 12))
	mod, ver, _ := Encode(data)
	chosen := -1
	for m := 0; m < 8; m++ {
		x := newMatrix(ver)
		x.functionPatterns(ver)
		x.place(interleave(dataCodewords(data, ver), ver))
		x.applyMask(m)
		x.formatBits(m)
		same := true
		for y := range mod {
			for xx := range mod {
				if mod[y][xx] != x.mod[y][xx] {
					same = false
				}
			}
		}
		if same {
			chosen = m
		}
	}
	if chosen < 0 {
		t.Fatal("the code is none of the eight")
	}
	best := 1 << 30
	for m := 0; m < 8; m++ {
		x := newMatrix(ver)
		x.functionPatterns(ver)
		x.place(interleave(dataCodewords(data, ver), ver))
		x.applyMask(m)
		x.formatBits(m)
		if p := x.penalty(); p < best {
			best = p
		}
	}
	x := newMatrix(ver)
	x.functionPatterns(ver)
	x.place(interleave(dataCodewords(data, ver), ver))
	x.applyMask(chosen)
	x.formatBits(chosen)
	if x.penalty() != best {
		t.Fatalf("mask %d is not the best", chosen)
	}
}

// Fingerprints of codes: web/static/scripts/qr.js must make the same ones (open a page that loads it, encode these inputs and hash the rows
// of 0 and 1 with SHA-256; the first six bytes in hex are these). If either encoder changes, the other must follow.
func TestFingerprintsForTheScriptVersion(t *testing.T) {
	want := []struct {
		in          string
		ver         int
		fingerprint string
	}{
		{"A", 1, "c70a942b2850"},
		{"hello world", 1, "09434ae2f1e0"},
		{strings.Repeat("0123456789", 30), 11, "a5a70ade96e8"},
		{strings.Repeat("peakroute ", 70), 18, "a92a4116fa0c"},
	}
	for _, w := range want {
		mod, ver, err := Encode([]byte(w.in))
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		for _, row := range mod {
			for _, d := range row {
				if d {
					sb.WriteByte('1')
				} else {
					sb.WriteByte('0')
				}
			}
		}
		h := sha256.Sum256([]byte(sb.String()))
		if got := hex.EncodeToString(h[:6]); ver != w.ver || got != w.fingerprint {
			t.Errorf("%d bytes: version %d fingerprint %s, want %d %s", len(w.in), ver, got, w.ver, w.fingerprint)
		}
	}
}
