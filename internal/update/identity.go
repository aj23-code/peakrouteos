package update

import (
	"fmt"
	"regexp"
	"strings"
)

// What a system is, besides its processors (Arch): the product (the router, or PeakRoute AP) and the board it is built for ("generic":
// any machine of its processors, such as any x86 PC; or one model of access point, whose kernel, device tree and disk are its own). A
// bundle goes only onto a system of the same product, board and processors (Manifest.Compatible), and the GitHub releases are looked
// through for that product's tags and the files of that processor and board only (FindRelease). Systems and bundles made before these
// existed say neither: they are the router, generic.
//
// A release's files are named for all of it: peakroute-<os|ap>-<arch>-<board>-<version>.<ext>, such as
// peakroute-os-amd64-generic-v0.10.0.update (the router on any x86 PC) or peakroute-ap-arm64-ubnt-u6plus-v0.1.0.update.
const (
	ProductRouter = "router"
	ProductAP     = "ap"
	BoardGeneric  = "generic"
	// boardPC is what the generic board was called by the builds of 2026-10-30 and 31 (it is read as generic)
	boardPC = "pc"
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
	if id.Board == "" || id.Board == boardPC {
		id.Board = BoardGeneric
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
	if id.Board == BoardGeneric {
		return product + " for a generic machine"
	}
	return product + " for the " + id.Board
}

// FilePrefix is how a release's files of this identity and processor begin: peakroute-<os|ap>-<arch>-<board>- (the router's product
// is "os" in file names, PeakRoute OS).
func (id Identity) FilePrefix(arch string) string {
	id = id.withDefaults()
	product := id.Product
	if product == ProductRouter {
		product = "os"
	}
	return "peakroute-" + product + "-" + arch + "-" + id.Board + "-"
}

// assetRank says how well a release's file name is this identity's update file for this processor: 2 named for it in full, 1 named
// as the router's were before the names said the processor and board (peakroute-os-v0.9.8.update: the router, generic, amd64, which
// is all there was), 0 not its file.
func (id Identity) assetRank(arch, name string) int {
	id = id.withDefaults()
	if !strings.HasSuffix(name, ".update") {
		return 0
	}
	if rest, ok := strings.CutPrefix(name, id.FilePrefix(arch)); ok && rest != ".update" {
		return 2
	}
	if id.Product == ProductRouter && id.Board == BoardGeneric && arch == "amd64" && legacyRouterAsset.MatchString(name) {
		return 1
	}
	return 0
}

var legacyRouterAsset = regexp.MustCompile(`^peakroute-os-v?[0-9][^/]*\.update$`)

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
