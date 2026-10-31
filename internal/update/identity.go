package update

import (
	"fmt"
	"regexp"
	"strings"
)

// What a system is, besides its processors (Arch): the product (the router, or PeakRoute AP) and the board it is built for (any x86
// machine, or one model of access point, whose kernel, device tree and disk are its own). A bundle goes only onto a system of the same
// product, board and processors (Manifest.Compatible), and the GitHub releases are looked through for that product's tags and that
// board's files only (FindRelease). Systems and bundles made before these existed say neither: they are the router on x86.
const (
	ProductRouter = "router"
	ProductAP     = "ap"
	BoardPC       = "pc"
)

// Identity is the product and board of a system or a bundle.
type Identity struct {
	Product string `json:"product"`
	Board   string `json:"board"`
}

var namePart = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// withDefaults fills in what a system or bundle from before the fields existed did not say.
func (id Identity) withDefaults() Identity {
	if id.Product == "" {
		id.Product = ProductRouter
	}
	if id.Board == "" {
		id.Board = BoardPC
	}
	return id
}

// valid says whether both are plain names (they become part of file names and patterns).
func (id Identity) valid() bool {
	return namePart.MatchString(id.Product) && namePart.MatchString(id.Board)
}

// Describe names the identity for a person: "the router for x86 machines", "PeakRoute AP for the ubnt-u6plus".
func (id Identity) Describe() string {
	id = id.withDefaults()
	product := "the router"
	if id.Product == ProductAP {
		product = "PeakRoute AP"
	} else if id.Product != ProductRouter {
		product = id.Product
	}
	if id.Board == BoardPC {
		return product + " for x86 machines"
	}
	return product + " for the " + id.Board
}

// assetPattern is the name of a release's update file for this identity. The router on x86 keeps the name it always had
// (peakroute-os-<version>.update); everything else is peakroute-<product>-<board>-<version>.update.
func (id Identity) assetPattern() *regexp.Regexp {
	id = id.withDefaults()
	if id.Product == ProductRouter && id.Board == BoardPC {
		return assetRe
	}
	return regexp.MustCompile(`^peakroute-` + regexp.QuoteMeta(id.Product) + `-` + regexp.QuoteMeta(id.Board) + `-.+\.update$`)
}

// tagVersion is the version a release tag stands for, when the tag belongs to this product: the router's tags are v1.2.3, every other
// product's are <product>-v1.2.3 (ap-v0.1.0), so that one repository holds the releases of each without one being taken for another.
func (id Identity) tagVersion(tag string) (Version, string, bool) {
	id = id.withDefaults()
	rest := tag
	if id.Product != ProductRouter {
		var ok bool
		if rest, ok = strings.CutPrefix(tag, id.Product+"-"); !ok {
			return Version{}, "", false
		}
	} else if !strings.HasPrefix(strings.TrimSpace(tag), "v") && !startsWithDigit(tag) {
		return Version{}, "", false // another product's tag
	}
	v, ok := ParseVersion(rest)
	return v, rest, ok
}

func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// compatibleIdentity is the part of Compatible about product and board.
func compatibleIdentity(bundle, system Identity) error {
	b, s := bundle.withDefaults(), system.withDefaults()
	if b.Product != s.Product || b.Board != s.Board {
		return fmt.Errorf("this update is for %s and this device is %s: it cannot be installed here", b.Describe(), s.Describe())
	}
	return nil
}
